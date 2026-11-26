package server

import (
	"fmt"
	"slices"
	"sync"
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

// Adds that overlap a running drain loop, from many goroutines at once. Every
// address is announced exactly once, whichever batch it lands in. This is the
// test the race detector needs: the tests above order each Add against the
// drain loop with their own channels, so they would pass with Add unlocked.
func TestAddedIPAnnouncerConcurrentAdds(t *testing.T) {
	const workers, perWorker = 8, 50
	var mu sync.Mutex
	got := make(map[string]int)
	var inFlight, maxInFlight atomic.Int32
	a := newAddedIPAnnouncer(func(iface string, ips []string) {
		n := inFlight.Add(1)
		for m := maxInFlight.Load(); n > m && !maxInFlight.CompareAndSwap(m, n); m = maxInFlight.Load() {
		}
		mu.Lock()
		for _, ip := range ips {
			got[iface+" "+ip]++
		}
		mu.Unlock()
		time.Sleep(100 * time.Microsecond)
		inFlight.Add(-1)
	})

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				a.Add("eth0", fmt.Sprintf("10.%d.%d.1/24", w, i))
			}
		}(w)
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		a.mu.Lock()
		idle := !a.running
		a.mu.Unlock()
		if n == workers*perWorker && idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("announced %d of %d addresses; drain running=%v", n, workers*perWorker, !idle)
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for key, n := range got {
		if n != 1 {
			t.Errorf("%s announced %d times, want once", key, n)
		}
	}
	if m := maxInFlight.Load(); m != 1 {
		t.Errorf("max concurrent sends = %d, want 1", m)
	}
}
