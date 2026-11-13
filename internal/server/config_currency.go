package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/pulselock"
	"github.com/syleron/pulseha/rpc"
)

// Config currency: whether this node's config is known to be the cluster's
// current one, and what to do when it is not (docs/TEST-PLAN.md #117).
//
// The config clock (configStamp) orders competing pushes, and it depends on
// every node holding the cluster's version. A node that has just restarted did
// not, for two reasons that compounded. Its stamp started at zero on every
// start, because the clock was never written to disk; and nothing told it the
// cluster's version until a peer happened to push a full config, which a peer
// only does on a mutation of its own or, if it is the coordinator, once a
// minute. The appliance mutates the config within a second of the daemon
// starting -- lb_api's reconcile runs from the unit's ExecStartPost -- so every
// restart landed a mutation in that window. The node accepted it, answered
// success, broadcast it at version 1, and every peer refused it as superseded.
// On the Active node the next inbound sync then reverted the change, leaving an
// address up on the interface that no config expected; on the Passive node no
// sync ever came, so the two nodes held different groups indefinitely while the
// node re-logged the same warning once a minute.
//
// Three things close it, and each covers a case the others cannot:
//
//   - The stamp is persisted with the config (config.Config.Stamp), so a restart
//     resumes the clock where it left off. On its own that only helps when
//     nothing changed while the node was down.
//   - A node pulls the config from whoever it has evidence is ahead of it: every
//     peer at startup, a peer that answers a broadcast with "superseded", and a
//     peer whose envelope carries a higher version. The last of these is what
//     makes a two-node cluster converge at all -- the hash-majority repair of
//     #103 cannot act with one peer, but a version comparison needs no majority,
//     because a higher version is not an opinion.
//   - Mutations wait for the node to be current, and fail if it is not. The
//     caller's success has to mean the change will propagate; a change the
//     cluster will refuse is refused here first, with a message that says why.
//
// A pull that fails leaves the node proceeding on what it holds. That is
// deliberate: a two-node cluster whose only peer is down must keep taking
// changes (ADR-0002), and the next envelope from the peer re-triggers the pull
// when it is back.

const (
	// configPullTimeout bounds one FetchConfig round trip. A healthy peer on
	// the same segment answers in tens of milliseconds; an unreachable one costs
	// the whole budget, and that budget is paid by a mutation waiting at the
	// gate, which on the appliance is lb_api's reconcile holding up the unit's
	// start job.
	configPullTimeout = 3 * time.Second

	// configCurrencyWait is how long a mutation waits to be allowed through.
	// Normally the wait is one pull, well under a second. The ceiling exists so
	// that a pull that cannot complete -- a peer that accepts the connection and
	// never answers -- turns into a refused mutation rather than a hung caller.
	configCurrencyWait = 10 * time.Second

	// configPullInterval floors how often envelope evidence can start a pull
	// from the same peer. Envelopes arrive every health-check interval, and a
	// peer whose config cannot be fetched but whose envelopes still arrive would
	// otherwise cost a pull per envelope.
	configPullInterval = 5 * time.Second

	configStaleSinceStart   = "this node has not checked its config against its peers since it started"
	configStaleSuperseded   = "a peer holds a newer config than this node"
	configStaleFromEnvelope = "a peer reports a newer config version than this node holds"
)

// errConfigNotCurrent is the refusal a mutation gets while the node is not
// current. It is what the caller sees over the socket, so it says what to do.
var errConfigNotCurrent = errors.New("config change refused: this node's config is not current with the cluster")

// configCurrency is the gate. `stale` is non-nil while the node is not known to
// be current and is closed, then dropped, when it becomes so; waiters block on
// it. `pulling` is the single-flight flag for the pull that will settle it. Both
// live under one mutex so a pull cannot finish between a trigger marking the
// node stale and finding a pull already in flight -- with two separate guards
// that window left the gate shut with nothing to open it.
type configCurrency struct {
	mu       pulselock.Mutex
	stale    chan struct{}
	reason   string
	pulling  bool
	lastPull map[string]time.Time
}

// markConfigStale shuts the gate. Idempotent; a later reason replaces the
// earlier one in the message a refused caller sees.
func (s *Server) markConfigStale(reason string) {
	s.currency.mu.Lock()
	defer s.currency.mu.Unlock()
	s.markConfigStaleLocked(reason)
}

func (s *Server) markConfigStaleLocked(reason string) {
	if s.currency.stale == nil {
		s.currency.stale = make(chan struct{})
	}
	s.currency.reason = reason
}

// markConfigCurrent opens the gate and releases every waiter.
func (s *Server) markConfigCurrent() {
	s.currency.mu.Lock()
	defer s.currency.mu.Unlock()
	s.markConfigCurrentLocked()
}

func (s *Server) markConfigCurrentLocked() {
	if s.currency.stale != nil {
		close(s.currency.stale)
		s.currency.stale = nil
	}
	s.currency.reason = ""
}

// configIsCurrent reports whether a mutation would be let through right now.
func (s *Server) configIsCurrent() bool {
	s.currency.mu.Lock()
	defer s.currency.mu.Unlock()
	return s.currency.stale == nil
}

// awaitConfigCurrent is what every config mutation calls before taking the
// server lock. It returns at once on a current node; otherwise it waits for the
// pull that is settling the question, and refuses the mutation if that takes
// longer than configCurrencyWait or than the caller is prepared to wait.
//
// Before the lock, and it has to be: the pull it waits on applies the peer's
// config through ConfigSync, which takes s.Lock() itself.
//
// A Server built as a literal -- every test in this package -- has a nil
// channel here and is current, so the gate is inert where nothing has shut it.
func (s *Server) awaitConfigCurrent(ctx context.Context) error {
	s.currency.mu.Lock()
	stale, reason := s.currency.stale, s.currency.reason
	s.currency.mu.Unlock()
	if stale == nil {
		return nil
	}

	timer := time.NewTimer(configCurrencyWait)
	defer timer.Stop()
	select {
	case <-stale:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w (%s); the change would not propagate, and the caller gave up waiting: %v",
			errConfigNotCurrent, reason, ctx.Err())
	case <-timer.C:
		return fmt.Errorf("%w (%s); the change would not propagate -- retry shortly",
			errConfigNotCurrent, reason)
	}
}

// startConfigPull shuts the gate and starts the pull that will open it again,
// unless one is already running -- that one settles the gate when it finishes,
// whichever trigger shut it. `floor` rate-limits repeat pulls from the same
// peer; zero means none.
func (s *Server) startConfigPull(peerID, reason string, floor time.Duration) {
	if peerID == "" {
		return
	}

	s.currency.mu.Lock()
	if floor > 0 {
		if last, ok := s.currency.lastPull[peerID]; ok && time.Since(last) < floor {
			s.currency.mu.Unlock()
			return
		}
	}
	s.markConfigStaleLocked(reason)
	if s.currency.pulling {
		s.currency.mu.Unlock()
		return
	}
	s.currency.pulling = true
	if s.currency.lastPull == nil {
		s.currency.lastPull = map[string]time.Time{}
	}
	s.currency.lastPull[peerID] = time.Now()
	s.currency.mu.Unlock()

	// On the repair wait group so the test harnesses' cleanup waits it out, the
	// same as the hash-driven repair it is a sibling of.
	s.configRepairs.Add(1)
	go func() {
		defer s.configRepairs.Done()
		defer s.settleConfigCurrency()

		applied, stamp, err := s.pullConfigFrom(peerID)
		switch {
		case err != nil:
			s.logger.Warn("CONFIG_PULL: could not pull the peer's config; proceeding on the config held",
				"peer", peerID, "reason", reason, "error", err)
		case applied:
			s.logger.Info("CONFIG_PULL: adopted the peer's newer config",
				"peer", peerID, "reason", reason, "version", stamp.version, "origin", stamp.origin)
		default:
			s.logger.Debug("CONFIG_PULL: the peer's config is not newer than this node's",
				"peer", peerID, "reason", reason, "peerVersion", stamp.version)
		}
	}()
}

// settleConfigCurrency ends a pull: the node is current, whatever the pull
// found. A pull that applied nothing found nothing newer; a pull that failed
// leaves the node proceeding on what it holds, which the header of this file
// explains. Under the one mutex, so no trigger can slip between the flag and
// the gate.
func (s *Server) settleConfigCurrency() {
	s.currency.mu.Lock()
	defer s.currency.mu.Unlock()
	s.currency.pulling = false
	s.markConfigCurrentLocked()
}

// noteConfigVersion is the envelope trigger: a sender whose version is above
// this node's is ahead, and this node pulls from it. Nothing on an envelope is
// applied directly; the envelope is evidence, the pull is the repair.
//
// No majority is consulted, unlike the hash detector beside it. A hash mismatch
// is symmetric and says nothing about which side is wrong; a higher version
// says the sender applied content this node never saw, and the receiver would
// have adopted that content had the broadcast reached it. Pulling it is the
// broadcast arriving late.
func (s *Server) noteConfigVersion(senderID string, incoming configStamp) {
	if senderID == "" || incoming.isEmpty() {
		return
	}
	if !configIsNewer(incoming, s.loadConfigStamp()) {
		return
	}
	s.startConfigPull(senderID, configStaleFromEnvelope, configPullInterval)
}

// pullConfigFrom fetches one peer's config and applies it if it is newer than
// what this node holds. The decision is made here, from the payload's own stamp,
// and then again by ConfigSync under the lock.
//
// FetchConfig answers with `repair_for` naming the asker, which lets a hash
// repair apply at an equal generation. A pull on version evidence must not use
// that exemption: the evidence can be stale by the time the fetch returns, and
// an addressed payload would apply a config this node has since moved past. So
// the tag is stripped and the ordinary guard decides.
func (s *Server) pullConfigFrom(peerID string) (applied bool, stamp configStamp, err error) {
	s.RLock()
	node := s.config.Nodes[peerID]
	var peer config.Node
	if node != nil {
		peer = *node
	}
	s.RUnlock()
	if node == nil {
		return false, configStamp{}, fmt.Errorf("peer %s is not in the local config", peerID)
	}

	payload, stamp, err := s.fetchConfigFrom(peerID, &peer)
	if err != nil {
		return false, configStamp{}, err
	}
	if stamp.isEmpty() || !configIsNewer(stamp, s.loadConfigStamp()) {
		return false, stamp, nil
	}
	if err := s.applyPulledConfig(peerID, payload); err != nil {
		return false, stamp, err
	}
	return true, stamp, nil
}

// fetchConfigFrom is the FetchConfig round trip, returning the payload and the
// stamp it carries.
func (s *Server) fetchConfigFrom(peerID string, node *config.Node) ([]byte, configStamp, error) {
	s.RLock()
	localID, _ := s.config.GetLocalNodeUUID()
	s.RUnlock()
	if localID == "" {
		return nil, configStamp{}, errors.New("this node has no identity to ask with")
	}

	client, err := s.getPeerClient(peerID, node)
	if err != nil {
		return nil, configStamp{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), configPullTimeout)
	defer cancel()
	resp, err := client.Server().FetchConfig(ctx, &rpc.FetchConfigRequest{RequesterId: localID})
	if err != nil {
		return nil, configStamp{}, err
	}
	if resp == nil || !resp.Success || len(resp.Config) == 0 {
		message := "no config returned"
		if resp != nil && resp.Message != "" {
			message = resp.Message
		}
		return nil, configStamp{}, fmt.Errorf("the peer would not serve its config: %s", message)
	}
	return resp.Config, payloadStamp(resp.Config), nil
}

// applyPulledConfig hands a fetched config to the ordinary inbound path, minus
// the repair tag, and reports whether it was applied.
func (s *Server) applyPulledConfig(peerID string, payload []byte) error {
	stripped, err := stripRepairTag(payload)
	if err != nil {
		return fmt.Errorf("could not prepare the peer's config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), configSyncTimeoutFor(len(stripped)))
	defer cancel()
	resp, err := s.ConfigSync(ctx, &rpc.ConfigSyncRequest{Config: stripped})
	switch {
	case err != nil:
		return fmt.Errorf("applying the peer's config failed: %w", err)
	case resp == nil || !resp.Success:
		message := ""
		if resp != nil {
			message = resp.Message
		}
		return fmt.Errorf("the peer's config was not applied: %s", message)
	case resp.Message == supersededConfigMessage || resp.Message == configAlreadyHeldMessage:
		// Decided again under the lock, and this node moved past the payload in
		// between. Not an error: the node is at least as current as the peer.
		s.logger.Debug("CONFIG_PULL: the peer's config was already held by the time it was applied",
			"peer", peerID, "message", resp.Message)
	}
	return nil
}

// payloadStamp reads the stamp a full-config payload carries, with the same
// origin fallback ConfigSync applies.
func payloadStamp(payload []byte) configStamp {
	var e struct {
		SenderID      string `json:"sender_id"`
		ConfigVersion int64  `json:"config_version"`
		ConfigOrigin  string `json:"config_origin"`
	}
	if err := json.Unmarshal(payload, &e); err != nil {
		return configStamp{}
	}
	origin := e.ConfigOrigin
	if origin == "" {
		origin = e.SenderID
	}
	return configStamp{version: e.ConfigVersion, origin: origin}
}

// stripRepairTag removes `repair_for` from a payload so ConfigSync's generation
// guard applies to it unexempted.
func stripRepairTag(payload []byte) ([]byte, error) {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, err
	}
	if _, tagged := decoded["repair_for"]; !tagged {
		return payload, nil
	}
	delete(decoded, "repair_for")
	return json.Marshal(decoded)
}

// startStartupConfigSync shuts the gate and asks every peer for its config, so
// the first mutation after a restart is taken against the cluster's version
// rather than the disk's. Called from Start, which holds the server lock; the
// goroutine blocks on the read lock until Start returns, which is the order it
// needs anyway.
func (s *Server) startStartupConfigSync() {
	s.currency.mu.Lock()
	s.markConfigStaleLocked(configStaleSinceStart)
	if s.currency.pulling {
		s.currency.mu.Unlock()
		return
	}
	s.currency.pulling = true
	s.currency.mu.Unlock()

	s.configRepairs.Add(1)
	go func() {
		defer s.configRepairs.Done()
		defer s.settleConfigCurrency()
		s.syncConfigAtStartup()
	}()
}

// syncConfigAtStartup fans FetchConfig out to every peer, adopts the newest
// config any of them holds, and re-sends this node's own config if a peer turns
// out to be behind it. The node that is ahead pushes, the node that is behind
// pulls; both directions are needed, because a node can die between committing
// a mutation and delivering it.
func (s *Server) syncConfigAtStartup() {
	s.RLock()
	localID, _ := s.config.GetLocalNodeUUID()
	peers := make(map[string]*config.Node, len(s.config.Nodes))
	for id, node := range s.config.Nodes {
		if id == localID || node == nil {
			continue
		}
		copied := *node
		peers[id] = &copied
	}
	s.RUnlock()

	if len(peers) == 0 {
		return
	}
	held := s.loadConfigStamp()

	type answer struct {
		id      string
		payload []byte
		stamp   configStamp
		err     error
	}
	answers := make(chan answer, len(peers))
	for id, node := range peers {
		go func(id string, node *config.Node) {
			payload, stamp, err := s.fetchConfigFrom(id, node)
			answers <- answer{id: id, payload: payload, stamp: stamp, err: err}
		}(id, node)
	}

	var (
		newest    answer
		reachable int
		behind    []string
	)
	for range peers {
		a := <-answers
		if a.err != nil {
			s.logger.Debug("CONFIG_STARTUP: peer did not serve its config", "peer", a.id, "error", a.err)
			continue
		}
		reachable++
		if a.stamp.isEmpty() {
			continue
		}
		if configIsNewer(a.stamp, held) && (newest.payload == nil || configIsNewer(a.stamp, newest.stamp)) {
			newest = a
		}
		if !held.isEmpty() && configIsNewer(held, a.stamp) {
			behind = append(behind, a.id)
		}
	}
	sort.Strings(behind)

	if reachable == 0 {
		s.logger.Warn("CONFIG_STARTUP: no peer reachable; proceeding on the config held, "+
			"and pulling from the first peer whose envelope shows a newer version",
			"version", held.version, "peers", len(peers))
		return
	}

	if newest.payload != nil {
		if err := s.applyPulledConfig(newest.id, newest.payload); err != nil {
			s.logger.Warn("CONFIG_STARTUP: could not adopt the newer config a peer holds; "+
				"proceeding on the config held", "peer", newest.id,
				"peerVersion", newest.stamp.version, "version", held.version, "error", err)
		} else {
			s.logger.Info("CONFIG_STARTUP: adopted a peer's newer config",
				"peer", newest.id, "version", newest.stamp.version, "origin", newest.stamp.origin,
				"wasVersion", held.version)
		}
	} else {
		s.logger.Info("CONFIG_STARTUP: config is current with every reachable peer",
			"version", held.version, "peersReached", reachable)
	}

	if len(behind) > 0 {
		s.logger.Info("CONFIG_STARTUP: peers hold an older config than this node; re-sending",
			"peers", behind, "version", s.loadConfigStamp().version)
		s.requestConfigBroadcast()
	}
}
