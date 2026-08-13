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

// A reconfigure must not install a config the world has moved past.
//
// Reconfigure reads the file and *then* swaps s.config, and the read is not
// inside the lock the swap takes. ConfigSync saves and swaps under that same
// lock, so a sync landing between a reconfigure's read and its swap is undone
// in memory by a snapshot taken before it existed:
//
//	sync #1 saves 100, spawns the reconfigure
//	the reconfigure reads the file          -> 100
//	sync #2 saves 120 and installs it       -> memory 120, disk 120
//	the reconfigure takes the lock, swaps   -> memory 100, disk 120
//
// The node then serves a config older than the one on its own disk and keeps
// doing so until something triggers another reconfigure, and it will broadcast
// that stale config as its own. Instrumented on 2026-08-07: disk=120, mem=100.
//
// This is what failed CI on 03ca13d as TestUnversionedConfigSyncStillApplies
// reading 100 where it wanted 120. That test had been passing by luck — its
// assertion usually beat the swap — so the defect is much older than the run
// that caught it.
func TestAsyncReconfigureDoesNotRevertANewerConfigSync(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	states := map[string]membership.MemberStatus{
		localID: membership.StatusActive,
		peerID:  membership.StatusActive,
	}
	sync := func(addresses int, version int64) {
		t.Helper()
		payload, err := buildFullConfigPayload(peerConfigWithGroup(s, "group1", addresses), states, 1,
			peerID, peerID, configStamp{version: version, origin: peerID})
		if err != nil {
			t.Fatalf("buildFullConfigPayload(%d): %v", addresses, err)
		}
		resp, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload})
		if err != nil {
			t.Fatalf("ConfigSync(%d addresses): %v", addresses, err)
		}
		if !resp.GetSuccess() {
			t.Fatalf("ConfigSync(%d addresses) refused: %s", addresses, resp.GetMessage())
		}
	}

	// No wait between them: the second must land inside the first's reconfigure,
	// which is the whole window under test.
	sync(100, 42)
	sync(120, 43)

	s.awaitAsyncReconfigures()

	if got := groupIPCount(s, "group1"); got != 120 {
		t.Errorf("group size after the reconfigure = %d, want 120 — a stale reload reverted "+
			"a newer ConfigSync; the node now serves an older config than its own disk", got)
	}
}

// The other half of that guard: a reload nothing superseded must be installed.
//
// Without this, "never install" is indistinguishable from the fix. Inverting the
// condition kills no other test in the package, because every one of them either
// drives Reconfigure through a ConfigSync that already installed the payload
// itself, or runs under the harness's PULSEHA_TEST, where config.Load returns
// before touching the disk and a reload is a content no-op. Both make the swap
// unobservable, so both would pass a Reconfigure that threw its reload away and
// left the node serving a config older than its own disk — the same end state as
// defect #67, reached from the opposite direction.
func TestReconfigureInstallsAReloadNothingSuperseded(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	// Turning PULSEHA_TEST off is what gives this test its teeth: it is the flag
	// that makes config.Load skip the disk read. Validate's full path runs as a
	// result, which the harness config satisfies — the local node is present in
	// Nodes and every interval is above its minimum.
	t.Setenv("PULSEHA_TEST", "")

	// Written straight to the file rather than through a ConfigSync. A sync
	// installs its own payload in memory, which leaves the reload with nothing to
	// carry and proves nothing about whether the result was installed.
	onDisk := peerConfigWithGroup(s, "group1", 150)
	if err := onDisk.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := groupIPCount(s, "group1"); got == 150 {
		t.Fatalf("memory already holds the config written to disk; the reload has nothing to install")
	}

	// The rebind that follows the swap fails, by design: the harness addresses the
	// node in TEST-NET-1 so no socket outlives the test. The swap happens well
	// before it, so the error is expected rather than the subject — it is reported
	// only if the assertion below fails, where it would be the likelier cause.
	reconfigureErr := s.Reconfigure()

	if got := groupIPCount(s, "group1"); got != 150 {
		t.Errorf("group size after a reconfigure nothing superseded = %d, want 150 — the "+
			"reload was discarded and the node keeps serving a config older than its own "+
			"disk (Reconfigure returned %v)", got, reconfigureErr)
	}
}
