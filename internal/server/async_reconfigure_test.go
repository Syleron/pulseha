package server

import (
	"context"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

// A full ConfigSync replies before the Reconfigure it spawns has re-read the
// config file, and every harness in this package points the package-level
// config.CONFIG_LOCATION at its own t.TempDir(). A test that returns while that
// goroutine is still inside config.Load() therefore lets the *next* test's setup
// write the global the goroutine is reading — a genuine data race, in the
// harness rather than in the daemon, which the detector flags intermittently.
// CI caught it on `34b854e` between TestApplyingAPeerConfigAdoptsItsVersion's
// leaked goroutine and TestEqualVersionsFromTwoSendersConverge...'s setup.
//
// onAsyncReconfigure alone does not close it: it makes one reconfigure
// observable to a test that chooses to wait, so every ConfigSync call site has
// to remember to, and thirteen of the fourteen in this package did not.
// awaitAsyncReconfigures is the bound the harness can apply once, for all of
// them — the goroutine's lifetime never outlives the test that started it.
//
// The assertion is that the wait genuinely blocks. A no-op that returns
// immediately would satisfy any test that merely called it and then checked the
// world had settled, because the goroutine usually wins that race anyway.
func TestAwaitAsyncReconfiguresBlocksUntilTheSpawnedReconfigureReturns(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	// Hold the goroutine open at its very end, after Reconfigure has returned,
	// so the only thing keeping the wait blocked is the tracking itself.
	release := make(chan struct{})
	reconfigured := make(chan struct{})
	s.onAsyncReconfigure = func() {
		close(reconfigured)
		<-release
	}

	states := map[string]membership.MemberStatus{
		localID: membership.StatusActive,
		peerID:  membership.StatusActive,
	}
	payload, err := buildFullConfigPayload(peerConfigWithGroup(s, "group1", 3), states, 1,
		peerID, peerID, configStamp{version: 200, origin: peerID})
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync: %v", err)
	}

	// Deliberately NOT waiting for the goroutine to be observed running first.
	// The count is taken before the spawn, so the wait must already be blocked
	// the instant ConfigSync returns — and this is the ordering the harness
	// cleanup depends on, since the leaking pattern is a test that returns the
	// moment ConfigSync does. Counting inside the goroutine would leave a window
	// here where the wait finds nothing to wait for.
	returned := make(chan struct{})
	go func() {
		s.awaitAsyncReconfigures()
		close(returned)
	}()

	select {
	case <-returned:
		close(release)
		t.Fatal("awaitAsyncReconfigures returned while a reconfigure was still in flight — " +
			"the next test's config.CONFIG_LOCATION write can race this goroutine's read")
	case <-time.After(100 * time.Millisecond):
	}

	// The goroutine must genuinely have run, or the block above proves only that
	// the counter was never decremented.
	select {
	case <-reconfigured:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the async reconfigure never ran; the block above proves nothing")
	}

	close(release)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("awaitAsyncReconfigures did not return after the reconfigure finished")
	}
}
