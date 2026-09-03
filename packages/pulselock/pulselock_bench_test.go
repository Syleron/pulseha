package pulselock

import (
	"sync"
	"sync/atomic"
	"testing"
)

// These benchmarks exist because the design rests on numbers rather than on
// judgement, and the numbers should be checkable by whoever next doubts them.
//
// The question they answer: exact reentrancy detection needs goroutine
// identity, and Go offers no cheap way to get it. If it were cheap, this
// package would do the exact thing in production too and there would be no
// two-mechanism split to explain. Run:
//
//	go test -bench . -benchtime 1000000x ./packages/pulselock/
//
// Recorded on an Apple M2 Pro, Go 1.27, over three runs of a million
// iterations. Absolute figures differ per machine; the ratios should not:
//
//	Goid                       1706-1735 ns/op   <- ~100x an uncontended Lock
//	BareLockUncontended         16.4-16.7 ns/op
//	PulselockLockUncontended    17.9-18.1 ns/op   <- +9%, i.e. +1.5ns
//	BareRLockContended            114-120 ns/op
//	PulselockRLockContended       117-135 ns/op   <- parity, within noise
//	BareMixedRealistic          92.3-93.7 ns/op
//	PulselockMixedRealistic    98.2-109.8 ns/op   <- ~+10% on 63 reads : 1 write
//
// The overhead is a fixed handful of nanoseconds on operations that guard
// netlink calls and gRPC round trips, which is why it is affordable and the
// ~1710ns exact path is not.
//
// Every benchmark below runs in production mode. Under `go test` the exact
// mechanism is on by default, which would measure goid rather than the thing
// the daemon runs.

// productionMode switches off goroutine tracking for a benchmark, so what is
// measured is what the daemon actually executes.
func productionMode(b *testing.B) {
	b.Helper()
	old := current.Load()
	c := *old
	c.exact = false
	current.Store(&c)
	b.Cleanup(func() { current.Store(old) })
}

// BenchmarkGoid is the measurement the whole design turns on: the cost of
// obtaining goroutine identity, against the ~16.5ns uncontended lock it would guard.
func BenchmarkGoid(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = goid()
	}
}

func BenchmarkBareLockUncontended(b *testing.B) {
	var m sync.RWMutex
	for i := 0; i < b.N; i++ {
		m.Lock()
		m.Unlock()
	}
}

func BenchmarkPulselockLockUncontended(b *testing.B) {
	productionMode(b)
	var m RWMutex
	for i := 0; i < b.N; i++ {
		m.Lock()
		m.Unlock()
	}
}

func BenchmarkBareRLockContended(b *testing.B) {
	var m sync.RWMutex
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.RLock()
			m.RUnlock()
		}
	})
}

// BenchmarkPulselockRLockContended is the one that had to come out flat.
// RLock is the health-check hot path, and an earlier design that used
// TryRLock as its fast path measured 3x a bare RWMutex — the CAS retries
// under read contention where a plain Add does not. Gating on writeHeld
// instead is what brought it back to parity.
func BenchmarkPulselockRLockContended(b *testing.B) {
	productionMode(b)
	var m RWMutex
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.RLock()
			m.RUnlock()
		}
	})
}

// BenchmarkBareMixedRealistic and its pulselock twin are the figure worth
// quoting: many readers with an occasional writer, which is the shape of the
// daemon's own traffic against the member list and the server lock.
func BenchmarkBareMixedRealistic(b *testing.B) {
	var m sync.RWMutex
	var n int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if atomic.AddInt64(&n, 1)%64 == 0 {
				m.Lock()
				m.Unlock()
			} else {
				m.RLock()
				m.RUnlock()
			}
		}
	})
}

func BenchmarkPulselockMixedRealistic(b *testing.B) {
	productionMode(b)
	var m RWMutex
	var n int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if atomic.AddInt64(&n, 1)%64 == 0 {
				m.Lock()
				m.Unlock()
			} else {
				m.RLock()
				m.RUnlock()
			}
		}
	})
}

// BenchmarkPulselockLockExactMode records what the test-mode mechanism costs,
// so nobody is tempted to ship it: this is the number that cannot go in the
// daemon.
func BenchmarkPulselockLockExactMode(b *testing.B) {
	var m RWMutex
	for i := 0; i < b.N; i++ {
		m.Lock()
		m.Unlock()
	}
}
