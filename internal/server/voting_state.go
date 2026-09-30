package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/syleron/pulseha/packages/config"
)

// A ballot is totally ordered even when two proposers choose the same counter.
// Promises and acceptances are durable before their acknowledgements are sent.
type voteBallot struct {
	Number uint64
	Node   string
}

func (a voteBallot) less(b voteBallot) bool {
	return a.Number < b.Number || a.Number == b.Number && a.Node < b.Node
}

type acceptedVote struct {
	Promise  voteBallot
	Accepted voteBallot
	Subject  string
}
type persistentVotes struct {
	Version    int
	Identity   string
	Epoch      int64
	Electorate []string
	Counter    uint64
	Slots      map[string]acceptedVote
}

func voteIdentity(cfg *config.Config) string {
	sum := sha256.Sum256([]byte(cfg.Pulse.LocalNode))
	return hex.EncodeToString(sum[:])
}

// loadVotesLocked is lazy so construction of a non-clustered daemon has no
// side effects. Test servers set voteStatePath to a file in t.TempDir().
func (s *Server) loadVotesLocked(cfg *config.Config) error {
	if s.voteStateErr != nil {
		return s.voteStateErr
	}
	identity := voteIdentity(cfg)
	if s.voteState != nil {
		if s.voteState.Identity != identity {
			return fmt.Errorf("voting identity changed; restart required")
		}
		return nil
	}
	if s.voteStatePath == "" {
		s.voteStatePath = filepath.Join(config.CONFIG_DIR, "votes-"+identity+".json")
	}
	data, err := os.ReadFile(s.voteStatePath)
	if os.IsNotExist(err) {
		s.voteState = &persistentVotes{Version: 1, Identity: identity, Slots: map[string]acceptedVote{}}
		return nil
	}
	if err != nil {
		s.voteStateErr = err
		return err
	}
	var state persistentVotes
	err = json.Unmarshal(data, &state)
	if err == nil && (state.Version != 1 || state.Identity != identity || state.Epoch < 0 || state.Slots == nil) {
		err = fmt.Errorf("invalid voting state")
	}
	if err == nil {
		for _, slot := range state.Slots {
			if slot.Promise.less(slot.Accepted) || slot.Accepted.Number > 0 && (slot.Subject == "" || slot.Accepted.Node == "") {
				err = fmt.Errorf("invalid accepted ballot")
				break
			}
		}
	}
	if err != nil {
		s.voteStateErr = err
		return err
	}
	s.voteState = &state
	s.voteEpochFloor.Store(max(0, state.Epoch-1))
	return nil
}

// persistVotesLocked uses a same-directory replacement and syncs both the
// contents and rename. Any ambiguous failure disables voting until restart.
func (s *Server) persistVotesLocked() (err error) {
	defer func() {
		if err != nil {
			s.voteStateErr = err
		}
	}()
	data, err := json.Marshal(s.voteState)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.voteStatePath)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".votes-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.voteStatePath); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Server) votingEpoch(cfg *config.Config) (int64, error) {
	s.voteMu.Lock()
	err := s.loadVotesLocked(cfg)
	s.voteMu.Unlock()
	if err != nil {
		return 0, err
	}
	return s.GetClusterEpoch() + 1, nil
}

func (s *Server) nextVoteBallot(cfg *config.Config) (uint64, error) {
	s.voteMu.Lock()
	defer s.voteMu.Unlock()
	if err := s.loadVotesLocked(cfg); err != nil {
		return 0, err
	}
	n := s.voteState.Counter
	for _, slot := range s.voteState.Slots {
		n = max(n, slot.Promise.Number)
	}
	if n == ^uint64(0) {
		return 0, fmt.Errorf("voting ballot exhausted")
	}
	s.voteState.Counter = n + 1
	if err := s.persistVotesLocked(); err != nil {
		return 0, err
	}
	return n + 1, nil
}

func (s *Server) observeVoteBallot(cfg *config.Config, n uint64) error {
	s.voteMu.Lock()
	defer s.voteMu.Unlock()
	if err := s.loadVotesLocked(cfg); err != nil {
		return err
	}
	if n <= s.voteState.Counter {
		return nil
	}
	s.voteState.Counter = n
	return s.persistVotesLocked()
}

// voteSlotLocked does not discard promises on a timeout. A new prepare round
// recovers the highest accepted value from a majority instead.
func (s *Server) voteSlotLocked(epoch int64, ids []string) error {
	if epoch < s.voteState.Epoch {
		return fmt.Errorf("proposal predates durable voting epoch")
	}
	if epoch == s.voteState.Epoch && !slices.Equal(ids, s.voteState.Electorate) {
		return fmt.Errorf("electorate changed within voting epoch")
	}
	if epoch > s.voteState.Epoch {
		s.voteState.Epoch = epoch
		s.voteState.Electorate = slices.Clone(ids)
		s.voteState.Slots = map[string]acceptedVote{}
		s.voteEpochFloor.Store(epoch - 1)
	}
	return nil
}
