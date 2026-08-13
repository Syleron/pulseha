package membership

import (
	"io"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
)

// Regression for the startup deadlock that CI caught the moment #74 made the
// integration tests actually run: TestClusterFormation hung for the full 2m
// timeout, with Server.Start blocked in startHealthChecker → IsRunning and the
// health-check goroutine blocked in performHealthChecks → GetClusterEpoch.
//
// Two locks taken in opposite orders. Server.Start holds the *server's* write
// lock for its whole body and probes IsRunning from inside it; performHealthChecks
// holds the *health checker's* write lock for its whole body and calls back into
// the server for the epoch and the state broadcast. All the first tick has to do
// is land before Server.Start returns, which is a question of how fast the rest of
// Start runs — hence green on a dev machine and wedged on a runner.
//
// The probe is the edge removed here: whether the checker is running is advisory,
// so it must be answerable without the lock a live pass holds.
func TestIsRunningDoesNotBlockOnAPassInFlight(t *testing.T) {
	h := NewHealthChecker(NewMemberList(nil, log.New(io.Discard)), log.New(io.Discard))
	h.ready.Store(true)

	// Stand in for a health-check pass: performHealthChecks holds this lock across
	// its whole body, including the calls back into the server.
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		h.Lock()
		defer h.Unlock()
		close(held)
		<-release
	}()
	<-held
	defer close(release)

	answered := make(chan bool, 1)
	go func() { answered <- h.IsRunning() }()

	select {
	case got := <-answered:
		if !got {
			t.Error("IsRunning() = false while the checker is ready and not stopped")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IsRunning() blocked on a health-check pass that is itself waiting on " +
			"the server lock the caller holds. Server.Start probes this from inside " +
			"s.Lock(), so the two orders deadlock and the daemon never finishes starting")
	}
}

// Stop's flags must still be observed by a caller that cannot take the lock,
// or the lock-free read above answers with a stale "running" forever.
func TestIsRunningObservesStopWithoutTheLock(t *testing.T) {
	h := NewHealthChecker(NewMemberList(nil, log.New(io.Discard)), log.New(io.Discard))
	h.ready.Store(true)

	if !h.IsRunning() {
		t.Fatal("setup: IsRunning() = false on a ready checker")
	}

	h.ready.Store(false)
	h.stopped.Store(true)

	if h.IsRunning() {
		t.Error("IsRunning() = true after the checker was stopped")
	}
}
