package server

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// A burst of adds runs one announcement at a time: addresses queued while a
// batch is out go together in the next batch, not as calls of their own. This
// is the bound: SendGARPBatch caps its own fan-out, but separate calls do not
// share that cap.
func TestAddedIPAnnouncerRunsOneBatchAtATime(t *testing.T) {
	release := make(chan struct{})
	type call struct {
		iface string
		ips   []string
	}
	calls := make(chan call, 8)
	var inFlight, maxInFlight atomic.Int32

	a := newAddedIPAnnouncer(func(iface string, ips []string) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		calls <- call{iface, slices.Clone(ips)}
		<-release
		inFlight.Add(-1)
	})

	a.Add("eth0", "10.0.0.1/24")
	first := <-calls
	if first.iface != "eth0" || !slices.Equal(first.ips, []string{"10.0.0.1/24"}) {
		t.Fatalf("first batch = %v on %q, want the isolated add sent at once", first.ips, first.iface)
	}

	// Queued while the first batch is held, including a duplicate.
	a.Add("eth0", "10.0.0.2/24")
	a.Add("eth0", "10.0.0.3/24")
	a.Add("eth0", "10.0.0.2/24")
	a.Add("eth1", "10.1.0.1/24")

	select {
	case extra := <-calls:
		t.Fatalf("sent %v on %q while another batch was out", extra.ips, extra.iface)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	want := []call{
		{"eth0", []string{"10.0.0.2/24", "10.0.0.3/24"}},
		{"eth1", []string{"10.1.0.1/24"}},
	}
	for _, w := range want {
		select {
		case got := <-calls:
			if got.iface != w.iface || !slices.Equal(got.ips, w.ips) {
				t.Errorf("batch = %v on %q, want %v on %q", got.ips, got.iface, w.ips, w.iface)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("batch for %q never sent", w.iface)
		}
	}
	if m := maxInFlight.Load(); m != 1 {
		t.Errorf("max concurrent sends = %d, want 1", m)
	}
}

// Once the queue empties, the drain loop exits, and a later add starts a new
// one. A lost wake-up here would leave an address queued and never announced.
func TestAddedIPAnnouncerRestartsAfterDraining(t *testing.T) {
	sent := make(chan string, 4)
	a := newAddedIPAnnouncer(func(iface string, ips []string) { sent <- ips[0] })

	for _, ip := range []string{"10.0.0.1/24", "10.0.0.2/24"} {
		a.Add("eth0", ip)
		select {
		case got := <-sent:
			if got != ip {
				t.Fatalf("sent %s, want %s", got, ip)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was never announced", ip)
		}
		// Wait for the loop to see the empty queue and exit before the next add.
		deadline := time.Now().Add(5 * time.Second)
		for {
			a.mu.Lock()
			running := a.running
			a.mu.Unlock()
			if !running {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("drain loop never exited on an empty queue")
			}
			time.Sleep(time.Millisecond)
		}
	}
}
