// Package pulselock provides drop-in replacements for sync.Mutex and
// sync.RWMutex that report a lock this daemon has wedged itself on.
//
// # Why this exists
//
// Go's mutexes are not reentrant, so a method holding a lock cannot call a
// sibling that takes the same lock. Nothing enforced that, and it produced nine
// deadlocks: docs/TEST-PLAN.md #32, #46, #56, #85, #87, plus RebalanceCluster,
// hasQuorumLocked, Server.PromoteNode and Member.RemoveIPs. Every
// one was a method calling a locking sibling on its own receiver, in its own
// file — which is to say the constraint was already local and already
// auditable, and was still missed nine times.
//
// The failure mode is what makes it worth instrumenting. #56 wedged the
// health-check goroutine while it held the write lock: no node was promoted, a
// 287-address group stayed down, and `ACTIVE_CHECK: Starting active node
// failure check` appeared zero times in six minutes. A deadlock here is silent,
// and silence is the reason these are found on live clusters rather than in the
// suite.
//
// # Two mechanisms, because exactness is not affordable in production
//
// Detecting reentrancy exactly needs goroutine identity, and Go offers no cheap
// way to get it: runtime.Stack costs ~1710ns against ~16.5ns for the uncontended
// lock it would guard — around 100x. Measured on an M2 Pro over three runs of a
// million iterations; absolute figures differ per machine, that ratio will not.
// See pulselock_bench_test.go, which is where the numbers are checkable.
//
// So:
//
//   - Under `go test`, identity is tracked and reentrancy panics immediately,
//     naming what was already held. Exact, and the cost is irrelevant.
//
//   - In production, an acquisition that stays blocked past ReportAfter dumps
//     every goroutine's stack to stderr and then keeps waiting. Behaviour is
//     therefore identical to a bare sync mutex — the daemon wedges exactly as
//     it would have — but it says so first.
//
// The production path is a wedge detector, not a reentrancy detector. It is
// less precise and catches strictly more: all nine historical shapes block
// forever, and so do lock-ordering cycles between two objects, which no
// reentrancy check can see.
//
// # The diagnostic goes to stderr, deliberately
//
// Not to the daemon's logger. TEST-PLAN #33/#61 record a Debug line in
// packages/network that could not reach the journal at any logging_level,
// because nothing ever calls SetLevel on that package's logger — which made the
// fix it was meant to evidence unverifiable live. This package is a leaf and
// would have the same problem. Under Type=simple with no StandardOutput
// override, stderr reaches journalctl unconditionally.
//
// # What it does not do
//
// Nothing about lock ordering under `go test` (the panic path is reentrancy
// only), and nothing about unsynchronised access — a field read without the
// lock is a data race, and -race is the tool for those.
package pulselock

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ReportAfter is how long an acquisition may stay blocked before it is treated
// as a wedge and reported.
//
// Sized against the daemon's own timescales rather than picked round: no
// legitimate critical section here should approach it, because the lock covers
// a state transition and never the network I/O around it (docs/adr/0004).
// Before that rule holds everywhere, the longest hold that still spans I/O is
// an announcement batch, capped at network.AnnounceBatchTimeout's 120s — so a
// report during that transition is informative rather than wrong, and the
// threshold is deliberately below it.
const ReportAfter = 30 * time.Second

// reportEvery bounds how often one lock re-reports. A wedge blocks every
// goroutine that arrives afterwards, and each would otherwise dump the full
// goroutine set; one dump per lock per interval is enough to diagnose it and
// leaves the journal readable.
const reportEvery = 5 * time.Minute

// settings is everything the mechanism's behaviour depends on, held together
// so it can be read and replaced as one value.
//
// One struct behind one atomic pointer rather than a handful of separate vars,
// and the reason is a race the tests found: a watchdog fires on the timer's
// goroutine and reads these, while a test that wants to drive the watchdog in
// milliseconds writes them. Separate mutable globals made that a data race —
// in the scaffolding rather than the daemon, but a real one, and papering over
// it with a plain bool read would have left the same hazard for anyone who
// later made the threshold configurable at runtime.
//
// The hot path pays one atomic pointer load, which also buys consistency: a
// caller cannot see the exact mechanism switched on with the production
// threshold still in place.
type settings struct {
	// exact selects goroutine-identity tracking, which panics on a reentrant
	// acquisition. True under `go test` and false in the daemon, because
	// identity costs ~1710ns to obtain — see the package comment.
	exact bool

	reportAfter time.Duration
	reportEvery time.Duration

	// sink is where diagnoses go: os.Stderr in the daemon, per the package
	// comment on why not the project logger.
	sink io.Writer
}

var current atomic.Pointer[settings]

func init() {
	// testing.Testing() reports whether the binary was built by `go test`, so
	// the exact mechanism turns itself on in every package's tests with no
	// build tag to remember to set.
	current.Store(&settings{
		exact:       testing.Testing(),
		reportAfter: ReportAfter,
		reportEvery: reportEvery,
		sink:        os.Stderr,
	})
}

// cfg returns the settings in force. Never nil: init runs before any lock can
// be taken.
func cfg() *settings { return current.Load() }

// report writes a wedge diagnosis to stderr: what blocked, for how long, and
// every goroutine's stack.
//
// All goroutines, not just the caller's. A time.AfterFunc callback runs on the
// timer's own goroutine, so a single-goroutine trace here would show the timer
// rather than the blocked caller — and capturing the caller's stack before
// blocking would put runtime.Stack's ~1710ns on every contended acquisition,
// which is the cost this design exists to avoid. A full dump is also the more
// useful artefact: a deadlock is only legible once you can see which goroutine
// holds what.
func report(sink io.Writer, kind string, waited time.Duration) {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		if len(buf) >= 8<<20 {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	fmt.Fprintf(sink,
		"\nPULSELOCK: %s has been blocked for %s — the daemon is probably deadlocked.\n"+
			"PULSELOCK: it will keep waiting; behaviour is unchanged from an uninstrumented mutex.\n"+
			"PULSELOCK: all goroutine stacks follow. Look for one that holds this lock and is\n"+
			"PULSELOCK: waiting on it again — that is a non-reentrant re-acquisition.\n%s\n",
		kind, waited, buf)
}

// watchdog arms a report for an acquisition about to block, and returns the
// function that cancels it. Only ever called once an acquisition is known to
// be contended, so its cost never lands on the fast path.
func (r *reporter) watchdog(c *settings, kind string) func() {
	r.armed.Add(1)
	start := time.Now()
	t := time.AfterFunc(c.reportAfter, func() {
		if r.claimReport(c.reportEvery) {
			report(c.sink, kind, time.Since(start))
		}
	})
	return func() { t.Stop() }
}

// reporter rate-limits one lock's reports.
type reporter struct {
	lastReport atomic.Int64 // unix nanos of the last report, 0 for never

	// armed counts watchdogs started on this lock. Incremented only on an
	// already-contended acquisition, so it costs nothing that matters — and it
	// pins the property the whole design rests on: an uncontended read must
	// arm nothing, because that is what keeps RLock at parity with a bare
	// RWMutex. Without this, a regression that armed a watchdog on every read
	// would break no test, only performance.
	armed atomic.Int64
}

func (r *reporter) claimReport(every time.Duration) bool {
	now := time.Now().UnixNano()
	for {
		last := r.lastReport.Load()
		if last != 0 && now-last < int64(every) {
			return false
		}
		if r.lastReport.CompareAndSwap(last, now) {
			return true
		}
	}
}

// goid returns the calling goroutine's id.
//
// Only called under `go test`. It parses runtime.Stack's header, which is the
// only way to get goroutine identity from outside the runtime, and it costs
// ~1710ns — see the package comment for why that confines it to tests.
func goid() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// "goroutine 123 [running]:"
	const prefix = "goroutine "
	if n <= len(prefix) {
		return 0
	}
	rest := buf[len(prefix):n]
	for i := 0; i < len(rest); i++ {
		if rest[i] == ' ' {
			id, err := strconv.ParseInt(string(rest[:i]), 10, 64)
			if err != nil {
				return 0
			}
			return id
		}
	}
	return 0
}

// Mutex is a sync.Mutex that reports a wedge instead of hanging silently.
//
// The zero value is ready to use, and the method set matches sync.Mutex, so a
// type that embeds sync.Mutex today can embed this instead without any call
// site changing.
type Mutex struct {
	inner sync.Mutex
	rep   reporter

	// owner is the goroutine holding the lock, and is maintained under `go
	// test` only — see the package comment.
	owner atomic.Int64
}

// Lock acquires the mutex.
//
// Under `go test` a re-acquisition by the goroutine that already holds it
// panics: sync.Mutex is not reentrant, so that goroutine could never proceed,
// and a panic names the defect where a hang would only hide it.
func (m *Mutex) Lock() {
	c := cfg()
	if c.exact {
		self := goid()
		if m.owner.Load() == self {
			panic("pulselock: reentrant Lock — this goroutine already holds this mutex. " +
				"A method holding the lock has called a sibling that takes it; sync.Mutex " +
				"is not reentrant, so this would deadlock. Split the callee into an " +
				"xLocked variant that assumes the lock is held.")
		}
		m.inner.Lock()
		m.owner.Store(self)
		return
	}

	if m.inner.TryLock() {
		return
	}
	cancel := m.rep.watchdog(c, "a pulselock.Mutex acquisition")
	m.inner.Lock()
	cancel()
}

// Unlock releases the mutex.
func (m *Mutex) Unlock() {
	if cfg().exact {
		m.owner.Store(0)
	}
	m.inner.Unlock()
}

// TryLock acquires the mutex if it is free, reporting whether it did.
//
// Present so that embedding this in place of sync.Mutex cannot break a caller
// that uses it; nothing in the daemon does today.
func (m *Mutex) TryLock() bool {
	if !m.inner.TryLock() {
		return false
	}
	if cfg().exact {
		m.owner.Store(goid())
	}
	return true
}

// RWMutex is a sync.RWMutex that reports a wedge instead of hanging silently.
//
// The zero value is ready to use, and the method set matches sync.RWMutex.
type RWMutex struct {
	inner sync.RWMutex
	rep   reporter

	// writeHeld says whether any goroutine holds the write lock. Read on the
	// RLock fast path, where it costs one atomic load, so a reader pays the
	// watchdog only when a writer is actually in — which is exactly the
	// write-then-read shape that Server.PromoteNode had. It is maintained in
	// production as well as under test, because it is what keeps the read path
	// at parity with a bare RWMutex under contention — an earlier design that
	// used TryRLock as the fast path measured 3x, because a CAS retries under
	// read contention where a plain Add does not.
	writeHeld atomic.Bool

	// writeOwner and readers are maintained under `go test` only.
	writeOwner atomic.Int64
	readersMu  sync.Mutex
	readers    map[int64]int
}

// Lock acquires the write lock.
//
// Under `go test` this panics if the calling goroutine already holds this
// mutex, for writing or for reading. Both are unconditional deadlocks:
// RWMutex is not reentrant, and a write lock cannot be granted while the same
// goroutine holds a read lock it will never release.
func (m *RWMutex) Lock() {
	c := cfg()
	if c.exact {
		self := goid()
		if m.writeOwner.Load() == self {
			panic("pulselock: reentrant Lock — this goroutine already holds this mutex " +
				"for writing. sync.RWMutex is not reentrant, so this would deadlock. " +
				"Split the callee into an xLocked variant that assumes the lock is held.")
		}
		if m.readCount(self) > 0 {
			panic("pulselock: Lock while holding RLock — this goroutine holds this mutex " +
				"for reading and is asking for it for writing. The write can never be " +
				"granted, because the read it is waiting behind is its own.")
		}
		m.inner.Lock()
		m.writeOwner.Store(self)
		m.writeHeld.Store(true)
		return
	}

	if m.inner.TryLock() {
		m.writeHeld.Store(true)
		return
	}
	cancel := m.rep.watchdog(c, "a pulselock.RWMutex write acquisition")
	m.inner.Lock()
	cancel()
	m.writeHeld.Store(true)
}

// Unlock releases the write lock.
func (m *RWMutex) Unlock() {
	m.writeHeld.Store(false)
	if cfg().exact {
		m.writeOwner.Store(0)
	}
	m.inner.Unlock()
}

// RLock acquires a read lock.
//
// Under `go test` this panics if the calling goroutine holds the write lock —
// an unconditional deadlock, and the shape Server.PromoteNode had. Taking a
// second read lock is only *conditionally* a deadlock: it wedges when a writer
// has queued between the two acquisitions and not otherwise, so it is reported
// to stderr as suspicious rather than panicked on.
func (m *RWMutex) RLock() {
	c := cfg()
	if c.exact {
		self := goid()
		if m.writeOwner.Load() == self {
			panic("pulselock: RLock while holding Lock — this goroutine holds this mutex " +
				"for writing and is asking for it for reading. sync.RWMutex is not " +
				"reentrant, so this would deadlock. Split the callee into an xLocked " +
				"variant that assumes the lock is held.")
		}
		if m.readCount(self) > 0 {
			fmt.Fprintf(c.sink,
				"PULSELOCK: recursive RLock — this goroutine already holds this mutex for "+
					"reading. Not fatal, because it only deadlocks when a writer queues "+
					"between the two acquisitions, but it is a lock taken twice on one path "+
					"and should be an xLocked call instead.\n")
		}
		m.inner.RLock()
		m.addRead(self, 1)
		return
	}

	// One atomic load on the uncontended path. Arming the watchdog when a
	// writer is in costs nothing that matters: this acquisition is about to
	// block regardless.
	if m.writeHeld.Load() {
		cancel := m.rep.watchdog(c, "a pulselock.RWMutex read acquisition")
		m.inner.RLock()
		cancel()
		return
	}
	m.inner.RLock()
}

// RUnlock releases a read lock.
func (m *RWMutex) RUnlock() {
	if cfg().exact {
		m.addRead(goid(), -1)
	}
	m.inner.RUnlock()
}

// TryLock acquires the write lock if it is free, reporting whether it did.
//
// Present so that embedding this in place of sync.RWMutex cannot break a
// caller that uses it; nothing in the daemon does today.
func (m *RWMutex) TryLock() bool {
	if !m.inner.TryLock() {
		return false
	}
	if cfg().exact {
		m.writeOwner.Store(goid())
	}
	m.writeHeld.Store(true)
	return true
}

// TryRLock acquires a read lock if it can, reporting whether it did.
func (m *RWMutex) TryRLock() bool {
	if !m.inner.TryRLock() {
		return false
	}
	if cfg().exact {
		m.addRead(goid(), 1)
	}
	return true
}

// RLocker returns a sync.Locker whose Lock and Unlock call RLock and RUnlock,
// matching sync.RWMutex.
func (m *RWMutex) RLocker() sync.Locker { return (*rlocker)(m) }

type rlocker RWMutex

func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }

// readCount reports how many read locks the given goroutine holds. Test-mode
// bookkeeping, so the map's own mutex is not a contention concern.
func (m *RWMutex) readCount(g int64) int {
	m.readersMu.Lock()
	defer m.readersMu.Unlock()
	return m.readers[g]
}

func (m *RWMutex) addRead(g int64, delta int) {
	m.readersMu.Lock()
	defer m.readersMu.Unlock()
	if m.readers == nil {
		m.readers = make(map[int64]int)
	}
	n := m.readers[g] + delta
	if n <= 0 {
		// Dropped rather than left at zero: a long-lived daemon under test
		// would otherwise accumulate an entry per goroutine that ever read.
		delete(m.readers, g)
		return
	}
	m.readers[g] = n
}
