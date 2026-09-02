package pulselock

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// safeBuf collects report output. The watchdog writes from the timer's
// goroutine while the test reads from its own, so the buffer needs its own
// lock — a plain sync.Mutex, because this is test scaffolding and not the thing
// under test.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// withProductionMode runs fn with the package configured as it is in the
// daemon — no goroutine tracking, wedges detected by duration — with the
// threshold shortened so a test need not wait 30 seconds. Returns whatever the
// wedge reported.
//
// This exists because testing.Testing() is true in this package's own tests,
// which would otherwise leave the entire production path unreachable: the
// mechanism that actually runs in the daemon would have no coverage at all.
func withProductionMode(t *testing.T, threshold time.Duration, fn func()) string {
	t.Helper()
	sink := &safeBuf{}
	withSettings(t, &settings{
		exact:       false, // the daemon's mechanism, not the test one
		reportAfter: threshold,
		reportEvery: cfg().reportEvery,
		sink:        sink,
	})
	fn()
	return sink.String()
}

// withSettings installs settings for one test and restores them afterwards.
// Swapping the whole struct at once is what keeps the watchdog goroutine's read
// race-free.
func withSettings(t *testing.T, s *settings) {
	t.Helper()
	old := current.Load()
	current.Store(s)
	t.Cleanup(func() { current.Store(old) })
}

// withReportEvery narrows the rate-limit window without disturbing anything
// else in force.
func withReportEvery(t *testing.T, every time.Duration) {
	t.Helper()
	c := *cfg()
	c.reportEvery = every
	withSettings(t, &c)
}

func mustPanic(t *testing.T, wantSubstr string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic mentioning %q, got none", wantSubstr)
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected a string panic, got %T: %v", r, r)
		}
		if !strings.Contains(msg, wantSubstr) {
			t.Fatalf("panic did not mention %q:\n%s", wantSubstr, msg)
		}
	}()
	fn()
}

// --- the historical shapes, as the mechanism must see them ---
//
// Each of these is a defect this codebase actually shipped. They are written
// against bare pulselock types here; the daemon-level reconstructions live with
// the types they belong to.

// TestReentrantLockOnAMutexPanics is #32's shape (config.Reload calling Save)
// and #87's (GetLocalNodeUUID calling ClusterCheck): a plain sync.Mutex taken
// twice on one path, which is always a deadlock.
func TestReentrantLockOnAMutexPanics(t *testing.T) {
	var m Mutex
	m.Lock()
	defer m.Unlock()
	mustPanic(t, "reentrant Lock", func() { m.Lock() })
}

// TestReentrantWriteLockPanics is #46's shape (RemoveMember redistributing
// under the member-list lock) and #85's (EnterMaintenance calling
// BringDownIPs).
func TestReentrantWriteLockPanics(t *testing.T) {
	var m RWMutex
	m.Lock()
	defer m.Unlock()
	mustPanic(t, "reentrant Lock", func() { m.Lock() })
}

// TestReadLockWhileHoldingWriteLockPanics is Server.PromoteNode's shape:
// s.Lock() with a deferred unlock, then s.GetClusterEpoch(), which takes
// s.RLock(). Also #56's, where the health-check tick reached IsRunning.
func TestReadLockWhileHoldingWriteLockPanics(t *testing.T) {
	var m RWMutex
	m.Lock()
	defer m.Unlock()
	mustPanic(t, "RLock while holding Lock", func() { m.RLock() })
}

// TestWriteLockWhileHoldingReadLockPanics has no instance in the history, and
// is covered because it is the one remaining unconditional deadlock: the write
// can never be granted, because the read it queues behind is the caller's own.
func TestWriteLockWhileHoldingReadLockPanics(t *testing.T) {
	var m RWMutex
	m.RLock()
	defer m.RUnlock()
	mustPanic(t, "Lock while holding RLock", func() { m.Lock() })
}

// TestRecursiveReadLockWarnsButDoesNotPanic pins the one case the mechanism
// must not treat as fatal. Two read locks on one path deadlock only when a
// writer queues between them, so calling it a defect outright would be wrong —
// but it is still a lock taken twice, so it is reported.
func TestRecursiveReadLockWarnsButDoesNotPanic(t *testing.T) {
	sink := &safeBuf{}
	c := *cfg()
	c.sink = sink
	withSettings(t, &c)

	var m RWMutex
	m.RLock()
	m.RLock()
	m.RUnlock()
	m.RUnlock()

	if got := sink.String(); !strings.Contains(got, "recursive RLock") {
		t.Fatalf("expected a recursive-RLock report, got %q", got)
	}
}

// --- what must NOT be reported ---

// TestAnAuxiliaryMutexUnderTheMainLockIsNotAViolation is the false positive
// that sank the static survey: 7 of its 10 candidates were a callee taking a
// *different* mutex on the same receiver. Server has nine mutexes and the eight
// auxiliary ones exist precisely so they can be taken under the main lock.
func TestAnAuxiliaryMutexUnderTheMainLockIsNotAViolation(t *testing.T) {
	type server struct {
		RWMutex              // the main lock, as internal/server.Server embeds it
		auxMu   Mutex        // peerBringUpMu, clientMutex, propagationMu, ...
		aux     atomic.Int64 // stands in for what the aux mutex guards
	}
	s := &server{}

	s.Lock()
	s.auxMu.Lock()
	s.aux.Add(1)
	s.auxMu.Unlock()
	s.Unlock()

	if s.aux.Load() != 1 {
		t.Fatal("the guarded work did not run")
	}
}

// TestTheSameLockOnTwoObjectsIsNotAViolation covers the other way a naive
// checker goes wrong: MemberList methods lock each Member in turn, and those
// are separate mutexes on separate objects.
func TestTheSameLockOnTwoObjectsIsNotAViolation(t *testing.T) {
	a, b := &RWMutex{}, &RWMutex{}
	a.Lock()
	b.Lock()
	b.Unlock()
	a.Unlock()
}

// TestSequentialAcquisitionOnOneGoroutineIsNotAViolation guards against
// tracking that forgets to clear the owner on Unlock — which would make every
// second acquisition on any goroutine look reentrant.
func TestSequentialAcquisitionOnOneGoroutineIsNotAViolation(t *testing.T) {
	var m RWMutex
	for i := 0; i < 100; i++ {
		m.Lock()
		m.Unlock()
		m.RLock()
		m.RUnlock()
	}
}

// TestConcurrentUseIsNotAViolation is the discrimination that matters most: the
// mechanism must distinguish "this goroutine already holds it" from "some other
// goroutine holds it", which is ordinary contention and the daemon's steady
// state.
func TestConcurrentUseIsNotAViolation(t *testing.T) {
	var m RWMutex
	var n int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if (i+j)%8 == 0 {
					m.Lock()
					n++
					m.Unlock()
				} else {
					m.RLock()
					_ = n
					m.RUnlock()
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestReadLocksNestAcrossGoroutines checks that per-goroutine read tracking
// does not confuse one goroutine's read lock for another's — several readers
// holding the lock at once is the normal case, not a nested acquisition.
func TestReadLocksNestAcrossGoroutines(t *testing.T) {
	var m RWMutex
	const readers = 16

	held := make(chan struct{}, readers)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.RLock()
			held <- struct{}{}
			<-release
			m.RUnlock()
		}()
	}
	for i := 0; i < readers; i++ {
		select {
		case <-held:
		case <-time.After(5 * time.Second):
			t.Fatal("readers could not hold the lock concurrently")
		}
	}
	close(release)
	wg.Wait()
}

// --- the production mechanism ---

// TestAWedgedAcquisitionIsReportedInProduction is the whole point of the
// production path. The daemon does not track goroutine identity, so a
// reentrant acquisition presents as an acquisition that never completes; the
// mechanism must say so rather than hanging in silence, which is what #56 did
// for six minutes.
func TestAWedgedAcquisitionIsReportedInProduction(t *testing.T) {
	var m RWMutex
	done := make(chan struct{})

	out := withProductionMode(t, 50*time.Millisecond, func() {
		m.Lock() // held for the whole test: nothing will release it in time
		go func() {
			defer close(done)
			m.Lock() // wedged, exactly as a re-entering goroutine would be
			m.Unlock()
		}()

		select {
		case <-done:
			t.Fatal("the second acquisition returned; it should have stayed blocked")
		case <-time.After(500 * time.Millisecond):
			// still blocked, which is the correct behaviour
		}
		m.Unlock()
		<-done
	})

	for _, want := range []string{
		"PULSELOCK",
		"has been blocked for",
		"probably deadlocked",
		"keep waiting",
		"write acquisition",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the wedge report did not mention %q; got:\n%s", want, firstLines(out, 12))
		}
	}
}

// TestTheWedgeReportNamesEveryGoroutine checks the artefact is actually
// diagnosable. A single-goroutine trace would show the timer that fired, not
// the goroutine that blocked, so the dump has to cover all of them — and it is
// only useful if the holder appears in it alongside the waiter.
func TestTheWedgeReportNamesEveryGoroutine(t *testing.T) {
	var m Mutex
	done := make(chan struct{})

	out := withProductionMode(t, 50*time.Millisecond, func() {
		m.Lock()
		go func() {
			defer close(done)
			m.Lock()
			m.Unlock()
		}()
		time.Sleep(300 * time.Millisecond)
		m.Unlock()
		<-done
	})

	if n := strings.Count(out, "goroutine "); n < 2 {
		t.Errorf("expected every goroutine's stack, found %d; got:\n%s", n, firstLines(out, 20))
	}
	if !strings.Contains(out, "pulselock.(*Mutex).Lock") {
		t.Errorf("the dump does not show the blocked acquisition:\n%s", firstLines(out, 20))
	}
}

// TestAContendedButHealthyAcquisitionIsNotReported is the negative control. A
// lock that is merely busy must stay silent, or a daemon under convergence load
// would report itself deadlocked continuously and the diagnostic would be
// worth nothing.
func TestAContendedButHealthyAcquisitionIsNotReported(t *testing.T) {
	var m RWMutex

	out := withProductionMode(t, 250*time.Millisecond, func() {
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 500; j++ {
					m.Lock()
					m.Unlock()
					m.RLock()
					m.RUnlock()
				}
			}()
		}
		wg.Wait()
	})

	if out != "" {
		t.Errorf("healthy contention was reported as a wedge:\n%s", firstLines(out, 12))
	}
}

// TestAWedgedReadIsReportedOnlyWhenAWriterHoldsIt pins the read fast path. A
// reader arms the watchdog only when writeHeld is set, which is what keeps
// RLock at parity with a bare RWMutex under contention — and write-then-read
// is exactly the shape it needs to catch, so gating on the writer loses
// nothing.
func TestAWedgedReadIsReportedOnlyWhenAWriterHoldsIt(t *testing.T) {
	var m RWMutex
	done := make(chan struct{})

	out := withProductionMode(t, 50*time.Millisecond, func() {
		m.Lock()
		go func() {
			defer close(done)
			m.RLock()
			m.RUnlock()
		}()
		time.Sleep(300 * time.Millisecond)
		m.Unlock()
		<-done
	})

	if !strings.Contains(out, "read acquisition") {
		t.Errorf("a read blocked behind a writer was not reported:\n%s", firstLines(out, 12))
	}
}

// TestOneWedgeReportsOncePerInterval keeps a wedge from burying the journal. A
// held lock blocks every goroutine that arrives after it, and each would
// otherwise dump the entire goroutine set.
func TestOneWedgeReportsOncePerInterval(t *testing.T) {
	var m Mutex
	var wg sync.WaitGroup

	withReportEvery(t, time.Hour) // one report, then silence
	out := withProductionMode(t, 50*time.Millisecond, func() {
		m.Lock()
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.Lock()
				m.Unlock()
			}()
		}
		time.Sleep(300 * time.Millisecond)
		m.Unlock()
		wg.Wait()
	})

	if n := strings.Count(out, "has been blocked for"); n != 1 {
		t.Errorf("expected exactly one report from eight blocked goroutines, got %d", n)
	}
}

// TestProductionModeDoesNotChangeBehaviour is the promise the design rests on:
// the daemon wedges exactly as an uninstrumented mutex would, so shipping this
// cannot alter what the daemon does — only what it says.
func TestProductionModeDoesNotChangeBehaviour(t *testing.T) {
	var m RWMutex
	var order []int
	var mu sync.Mutex

	withProductionMode(t, 50*time.Millisecond, func() {
		m.Lock()
		started := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			close(started)
			m.Lock()
			mu.Lock()
			order = append(order, 2)
			mu.Unlock()
			m.Unlock()
		}()
		<-started
		time.Sleep(200 * time.Millisecond) // long enough to have been reported
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
		m.Unlock()
		<-done
	})

	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("mutual exclusion was not preserved: %v", order)
	}
}

// --- drop-in compatibility ---
//
// The mechanism's affordability rests on it being a one-word change per
// declaration: ~330 existing call sites must compile untouched.

// TestEmbeddingPromotesTheLockMethods is the property that makes the swap a
// one-word edit. internal/server.Server embeds its mutex, so s.Lock() must keep
// resolving after the type under it changes.
func TestEmbeddingPromotesTheLockMethods(t *testing.T) {
	type server struct {
		RWMutex
		epoch int64
	}
	s := &server{}

	s.Lock() // promoted, exactly as with an embedded sync.RWMutex
	s.epoch++
	s.Unlock()

	s.RLock()
	got := s.epoch
	s.RUnlock()

	if got != 1 {
		t.Fatalf("epoch = %d, want 1", got)
	}
}

// TestTheZeroValueIsUsable matters because these types are embedded rather than
// constructed: nothing in the daemon calls a pulselock constructor, so a type
// that needs initialising could not be a drop-in at all.
func TestTheZeroValueIsUsable(t *testing.T) {
	var m Mutex
	m.Lock()
	m.Unlock()

	var rw RWMutex
	rw.Lock()
	rw.Unlock()
	rw.RLock()
	rw.RUnlock()
}

// TestTheMethodSetMatchesSync guards the drop-in property at compile time: if
// either type stops satisfying the interface a sync mutex satisfies, a call
// site somewhere in the daemon stops compiling.
func TestTheMethodSetMatchesSync(t *testing.T) {
	var _ sync.Locker = (*Mutex)(nil)
	var _ sync.Locker = (*RWMutex)(nil)

	type rwLocker interface {
		Lock()
		Unlock()
		RLock()
		RUnlock()
		TryLock() bool
		TryRLock() bool
		RLocker() sync.Locker
	}
	var _ rwLocker = (*RWMutex)(nil)
	var _ rwLocker = (*sync.RWMutex)(nil) // the shape being matched
}

// TestRLockerReadLocks covers the one promoted method with its own type behind
// it, since a broken RLocker would fail only at the call site that used it.
func TestRLockerReadLocks(t *testing.T) {
	var m RWMutex
	l := m.RLocker()
	l.Lock()
	l.Unlock()

	// It must be the *read* lock: two of these at once is legal.
	l.Lock()
	l2 := m.RLocker()
	l2.Lock()
	l2.Unlock()
	l.Unlock()
}

// TestTryLockTracksOwnershipToo guards a gap the tracking could easily have:
// TryLock acquires the same lock as Lock, so a reentrant acquisition after a
// successful TryLock is the same deadlock and must be caught the same way.
func TestTryLockTracksOwnershipToo(t *testing.T) {
	var m Mutex
	if !m.TryLock() {
		t.Fatal("TryLock failed on a free mutex")
	}
	defer m.Unlock()
	mustPanic(t, "reentrant Lock", func() { m.Lock() })
}

func TestTryLockOnAHeldMutexReportsFailure(t *testing.T) {
	var m Mutex
	m.Lock()
	defer m.Unlock()

	done := make(chan bool, 1)
	go func() { done <- m.TryLock() }()

	select {
	case got := <-done:
		if got {
			t.Fatal("TryLock succeeded on a held mutex")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TryLock blocked; it must not")
	}
}

// --- rate limiting, directly ---

func TestClaimReportRateLimits(t *testing.T) {
	var r reporter
	if !r.claimReport(time.Hour) {
		t.Fatal("the first report should be allowed")
	}
	if r.claimReport(time.Hour) {
		t.Fatal("a second report inside the interval should be suppressed")
	}
}

func TestClaimReportAllowsAgainAfterTheInterval(t *testing.T) {
	var r reporter
	if !r.claimReport(time.Millisecond) {
		t.Fatal("the first report should be allowed")
	}
	time.Sleep(20 * time.Millisecond)
	if !r.claimReport(time.Millisecond) {
		t.Fatal("a report after the interval should be allowed again")
	}
}

// TestClaimReportIsRaceFreeUnderConcurrency covers the compare-and-swap loop:
// a wedge is precisely the case where many goroutines reach this at once, and
// exactly one of them must win.
func TestClaimReportIsRaceFreeUnderConcurrency(t *testing.T) {
	var r reporter
	var won atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.claimReport(time.Hour) {
				won.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := won.Load(); got != 1 {
		t.Fatalf("%d goroutines claimed the report, want exactly 1", got)
	}
}

// --- goroutine identity ---

func TestGoidIsStableWithinAGoroutineAndDistinctAcross(t *testing.T) {
	self := goid()
	if self == 0 {
		t.Fatal("goid returned 0; the header parse is broken and all tracking is dead")
	}
	if again := goid(); again != self {
		t.Fatalf("goid changed within one goroutine: %d then %d", self, again)
	}

	other := make(chan int64, 1)
	go func() { other <- goid() }()
	if got := <-other; got == self {
		t.Fatalf("two goroutines reported the same id (%d)", got)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
		return strings.Join(lines, "\n") + "\n\t... (truncated)"
	}
	return s
}

var _ io.Writer = (*safeBuf)(nil)

// TestAnUncontendedAcquisitionArmsNoWatchdog pins the property that makes the
// production mechanism affordable at all.
//
// Reads gate on writeHeld precisely so an uncontended RLock pays one atomic
// load and nothing else. A regression that armed a watchdog per read would
// still be correct — it would just put a timer allocation on the health-check
// hot path — so no other test in this file can see it.
func TestAnUncontendedAcquisitionArmsNoWatchdog(t *testing.T) {
	var m RWMutex

	withProductionMode(t, time.Hour, func() {
		// The writes come first, deliberately. Reads gate on writeHeld, so a
		// released write lock that fails to clear the flag makes every
		// subsequent read arm a watchdog — and reads running before any write
		// would never see it, which is how an earlier draft of this test let
		// exactly that regression through.
		for i := 0; i < 1000; i++ {
			m.Lock()
			m.Unlock()
		}
		for i := 0; i < 1000; i++ {
			m.RLock()
			m.RUnlock()
		}
	})

	if got := m.rep.armed.Load(); got != 0 {
		t.Errorf("uncontended acquisitions armed %d watchdogs, want 0", got)
	}
}

// TestAReadBehindAWriterDoesArmAWatchdog is the other half: the gate must not
// be so tight that write-then-read — Server.PromoteNode's shape, and the one
// the read path exists to catch — slips past unwatched.
func TestAReadBehindAWriterDoesArmAWatchdog(t *testing.T) {
	var m RWMutex
	done := make(chan struct{})

	withProductionMode(t, time.Hour, func() {
		m.Lock()
		go func() {
			defer close(done)
			m.RLock()
			m.RUnlock()
		}()
		// Wait for the reader to actually be blocked, rather than sleeping and
		// hoping: it has arrived once it has armed its watchdog.
		deadline := time.Now().Add(5 * time.Second)
		for m.rep.armed.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		m.Unlock()
		<-done
	})

	if got := m.rep.armed.Load(); got != 1 {
		t.Errorf("a read blocked behind a writer armed %d watchdogs, want 1", got)
	}
}
