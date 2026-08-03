// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package server

import (
	"sync"
	"time"
)

// peerBringUpWindow is how long a newly added group address waits for company
// before its peer bring-up is sent.
//
// It bounds the added latency of a single add, and it is the whole of the
// coalescing for a burst: 250ms is far longer than the ~30ms an `add-ip` takes
// once #39 moved the fan-out off the request path, so a script or the
// appliance's own loop adding addresses back to back lands many per window,
// while an isolated add is delayed by a quarter of a second before a peer starts
// on it — against the ENFORCE pass, which is the fallback, taking up to 30s.
const peerBringUpWindow = 250 * time.Millisecond

// peerBringUpKey identifies one destination for a batch: a peer endpoint and the
// interface the addresses belong on. Two groups on different interfaces of the
// same peer are different destinations and cannot share a request.
type peerBringUpKey struct {
	ip    string
	port  string
	iface string
}

// peerBringUpBatch is the set of addresses waiting to be sent to one
// destination, and the timer that will send them.
type peerBringUpBatch struct {
	target peerBringUpTarget
	ips    []string
	seen   map[string]bool
}

// peerBringUpBatcher coalesces per-address peer bring-ups into one request per
// peer per window.
//
// docs/TEST-PLAN.md defect #37, the remainder after #39. #39 took the fan-out
// off the request path, so an add no longer costs the caller ~4s per peer, and
// resolving a single owner means only one node is asked — but the work itself is
// still shaped one address at a time: every `add-ip` opens its own gRPC
// connection, sends a one-address `BringUpIP`, and that peer runs its own
// arping for that one address. Run 23's 20-add burst is what this costs live —
// **56 of 60** peer bring-ups were refused with `connection refused`, because
// each add dialled a listener that the config-sync storm those same adds cause
// was cycling (#31). Per-address requests do not merely waste time; they
// multiply the number of chances to hit that window.
//
// Batching makes a burst one connection and one `SendGARPBatch` per window
// instead of one per address, and `SendGARPBatch` is already the parallel,
// kernel-checked announcement path (#4/#8, #33). It changes nothing about
// correctness: the address is committed and broadcast before any of this, and
// the peer's ENFORCE pass converges on the config regardless — that is why this
// is allowed to be best-effort and to run late.
type peerBringUpBatcher struct {
	mu         sync.Mutex
	flushAfter time.Duration
	// send is called off the caller's goroutine, once per flush, with the
	// batch's addresses in the order they were added.
	send    func(target peerBringUpTarget, ips []string)
	pending map[peerBringUpKey]*peerBringUpBatch
	// afterFunc is time.AfterFunc, replaced in tests that need to fire the
	// window deterministically rather than by sleeping.
	afterFunc func(time.Duration, func()) *time.Timer
}

func newPeerBringUpBatcher(flushAfter time.Duration, send func(peerBringUpTarget, []string)) *peerBringUpBatcher {
	return &peerBringUpBatcher{
		flushAfter: flushAfter,
		send:       send,
		pending:    make(map[peerBringUpKey]*peerBringUpBatch),
		afterFunc:  time.AfterFunc,
	}
}

// Add queues one address for one peer and returns immediately.
//
// The window runs from the **first** address of a batch and is not extended by
// later ones. A sliding window would be the obvious choice and is the wrong one
// here: under the 200-address burst that #37 was found in, every add would push
// the flush back and the peer would be told nothing until the burst ended. A
// fixed window bounds the delay for every address at `flushAfter`, and a burst
// longer than one window simply produces several batches.
func (b *peerBringUpBatcher) Add(target peerBringUpTarget, ip string) {
	if b == nil {
		return
	}
	key := peerBringUpKey{ip: target.ip, port: target.port, iface: target.iface}

	b.mu.Lock()
	batch, ok := b.pending[key]
	if !ok {
		batch = &peerBringUpBatch{target: target, seen: make(map[string]bool)}
		b.pending[key] = batch
	}
	if !batch.seen[ip] {
		batch.seen[ip] = true
		batch.ips = append(batch.ips, ip)
	}
	b.mu.Unlock()

	if !ok {
		b.afterFunc(b.flushAfter, func() { b.flush(key) })
	}
}

// flush takes the batch out of the pending map before sending it, so addresses
// added while the request is in flight start a new batch instead of being
// dropped into one nothing will ever send again.
func (b *peerBringUpBatcher) flush(key peerBringUpKey) {
	b.mu.Lock()
	batch := b.pending[key]
	delete(b.pending, key)
	b.mu.Unlock()

	if batch == nil || len(batch.ips) == 0 {
		return
	}
	b.send(batch.target, batch.ips)
}
