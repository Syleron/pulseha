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
	"testing"
	"time"
)

type sentBatch struct {
	target peerBringUpTarget
	ips    []string
}

// batcherHarness fires the window on demand instead of sleeping, so the tests
// say what they mean about ordering rather than racing a timer.
type batcherHarness struct {
	batcher *peerBringUpBatcher
	mu      sync.Mutex
	sent    []sentBatch
	fire    []func()
}

func newBatcherHarness() *batcherHarness {
	h := &batcherHarness{}
	h.batcher = newPeerBringUpBatcher(time.Hour, func(target peerBringUpTarget, ips []string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.sent = append(h.sent, sentBatch{target: target, ips: ips})
	})
	h.batcher.afterFunc = func(_ time.Duration, f func()) *time.Timer {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.fire = append(h.fire, f)
		return nil
	}
	return h
}

// fireAll runs every window that has been armed and not yet run.
func (h *batcherHarness) fireAll() {
	h.mu.Lock()
	pending := h.fire
	h.fire = nil
	h.mu.Unlock()
	for _, f := range pending {
		f()
	}
}

func (h *batcherHarness) batches() []sentBatch {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]sentBatch, len(h.sent))
	copy(out, h.sent)
	return out
}

func peerAt(host, ip, iface string) peerBringUpTarget {
	return peerBringUpTarget{hostname: host, ip: ip, port: "8080", iface: iface}
}

// TestBatcherCoalescesABurstIntoOneRequest is defect #37's remainder: a burst of
// adds was one gRPC connection and one arping per address.
func TestBatcherCoalescesABurstIntoOneRequest(t *testing.T) {
	h := newBatcherHarness()
	target := peerAt("node-2", "10.0.0.2", "eth0")

	for _, ip := range []string{"10.200.0.1/23", "10.200.0.2/23", "10.200.0.3/23"} {
		h.batcher.Add(target, ip)
	}
	if len(h.batches()) != 0 {
		t.Fatalf("nothing should be sent before the window fires, got %v", h.batches())
	}

	h.fireAll()

	batches := h.batches()
	if len(batches) != 1 {
		t.Fatalf("expected one request for the whole burst, got %d: %v", len(batches), batches)
	}
	if len(batches[0].ips) != 3 {
		t.Fatalf("expected all three addresses in the one request, got %v", batches[0].ips)
	}
	for i, want := range []string{"10.200.0.1/23", "10.200.0.2/23", "10.200.0.3/23"} {
		if batches[0].ips[i] != want {
			t.Fatalf("expected the order they were added, got %v", batches[0].ips)
		}
	}
}

// TestBatcherArmsOneWindowPerBatch guards the fixed window: a later address must
// not push the flush back, or a long burst would never reach the peer.
func TestBatcherArmsOneWindowPerBatch(t *testing.T) {
	h := newBatcherHarness()
	target := peerAt("node-2", "10.0.0.2", "eth0")

	h.batcher.Add(target, "10.200.0.1/23")
	h.batcher.Add(target, "10.200.0.2/23")

	h.mu.Lock()
	armed := len(h.fire)
	h.mu.Unlock()
	if armed != 1 {
		t.Fatalf("expected exactly one window armed for the batch, got %d", armed)
	}
}

func TestBatcherKeepsDestinationsApart(t *testing.T) {
	h := newBatcherHarness()
	nodeTwo := peerAt("node-2", "10.0.0.2", "eth0")
	nodeThree := peerAt("node-3", "10.0.0.3", "eth0")
	otherIface := peerAt("node-2", "10.0.0.2", "eth1")

	h.batcher.Add(nodeTwo, "10.200.0.1/23")
	h.batcher.Add(nodeThree, "10.200.0.2/23")
	h.batcher.Add(otherIface, "10.200.0.3/23")
	h.fireAll()

	batches := h.batches()
	if len(batches) != 3 {
		t.Fatalf("expected one request per peer/interface, got %d: %v", len(batches), batches)
	}
	for _, b := range batches {
		if len(b.ips) != 1 {
			t.Fatalf("addresses were mixed across destinations: %v", batches)
		}
	}
}

// TestBatcherStartsAFreshBatchAfterAFlush is the drop this design could easily
// have: an address added while a request is in flight must not land in a batch
// nothing will send again.
func TestBatcherStartsAFreshBatchAfterAFlush(t *testing.T) {
	h := newBatcherHarness()
	target := peerAt("node-2", "10.0.0.2", "eth0")

	h.batcher.Add(target, "10.200.0.1/23")
	h.fireAll()

	h.batcher.Add(target, "10.200.0.2/23")
	h.fireAll()

	batches := h.batches()
	if len(batches) != 2 {
		t.Fatalf("expected the second address to be sent in its own batch, got %v", batches)
	}
	if batches[1].ips[0] != "10.200.0.2/23" {
		t.Fatalf("second batch carried the wrong address: %v", batches[1].ips)
	}
}

func TestBatcherDropsDuplicateAddresses(t *testing.T) {
	h := newBatcherHarness()
	target := peerAt("node-2", "10.0.0.2", "eth0")

	h.batcher.Add(target, "10.200.0.1/23")
	h.batcher.Add(target, "10.200.0.1/23")
	h.fireAll()

	batches := h.batches()
	if len(batches) != 1 || len(batches[0].ips) != 1 {
		t.Fatalf("expected one address in one request, got %v", batches)
	}
}

// TestBatcherRealTimerFires covers the wiring the harness replaces — that a real
// window does eventually send.
func TestBatcherRealTimerFires(t *testing.T) {
	done := make(chan []string, 1)
	b := newPeerBringUpBatcher(10*time.Millisecond, func(_ peerBringUpTarget, ips []string) {
		done <- ips
	})

	b.Add(peerAt("node-2", "10.0.0.2", "eth0"), "10.200.0.1/23")
	b.Add(peerAt("node-2", "10.0.0.2", "eth0"), "10.200.0.2/23")

	select {
	case ips := <-done:
		if len(ips) != 2 {
			t.Fatalf("expected both addresses, got %v", ips)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the window never fired")
	}
}
