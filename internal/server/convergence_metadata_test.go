package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

// The epoch and its leader are one decision, so they have to move together.
// ConfigSync used to compare-and-write them bare on three of its four paths —
// the envelope-only branch took no server lock at all — while the config
// broadcaster reads both under RLock.
func TestAdoptConvergenceMetadataComparison(t *testing.T) {
	for _, tc := range []struct {
		name         string
		held         int64
		incoming     int64
		atLeast      bool
		wantAdopted  bool
		wantEpochNow int64
	}{
		{name: "strictly newer is adopted", held: 4, incoming: 5, wantAdopted: true, wantEpochNow: 5},
		{name: "equal is refused without atLeast", held: 4, incoming: 4, wantAdopted: false, wantEpochNow: 4},
		{name: "equal is adopted with atLeast", held: 4, incoming: 4, atLeast: true, wantAdopted: true, wantEpochNow: 4},
		{name: "older is refused", held: 4, incoming: 3, wantAdopted: false, wantEpochNow: 4},
		{name: "older is refused even with atLeast", held: 4, incoming: 3, atLeast: true, wantAdopted: false, wantEpochNow: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newConfigSyncTestServer(t, "node-local", "node-peer")
			s.clusterEpoch = tc.held
			s.leaderID = "old-leader"

			adopted := s.adoptConvergenceMetadata(tc.incoming, "new-leader", tc.atLeast)
			if adopted != tc.wantAdopted {
				t.Errorf("adopted = %v, want %v", adopted, tc.wantAdopted)
			}

			epoch, leader := s.convergenceMetadata()
			if epoch != tc.wantEpochNow {
				t.Errorf("epoch = %d, want %d", epoch, tc.wantEpochNow)
			}
			wantLeader := "old-leader"
			if tc.wantAdopted {
				wantLeader = "new-leader"
			}
			if leader != wantLeader {
				t.Errorf("leader = %q, want %q", leader, wantLeader)
			}
		})
	}
}

// The epoch must never regress. The unlocked version compared the incoming epoch
// against a snapshot taken earlier in ConfigSync, so a sync that lost the race
// still wrote its lower epoch over the winner's.
func TestAdoptConvergenceMetadataNeverRegressesTheEpoch(t *testing.T) {
	s, _ := newConfigSyncTestServer(t, "node-local", "node-peer")

	if !s.adoptConvergenceMetadata(9, "leader-9", true) {
		t.Fatal("expected epoch 9 to be adopted from 0")
	}
	if s.adoptConvergenceMetadata(2, "leader-2", true) {
		t.Error("expected epoch 2 to be refused against a held epoch of 9")
	}

	epoch, leader := s.convergenceMetadata()
	if epoch != 9 || leader != "leader-9" {
		t.Errorf("after a losing adopt: epoch=%d leader=%q, want 9 and leader-9", epoch, leader)
	}
}

// The invariant the pair exists for: a reader must never see a new epoch beside
// the previous epoch's leader. Concurrent adopts plus concurrent reads, so -race
// has both sides to pair up; the assertion is that every observed combination is
// one that was actually written together.
func TestConvergenceMetadataIsNeverObservedMismatched(t *testing.T) {
	s, _ := newConfigSyncTestServer(t, "node-local", "node-peer")

	const adopts = 200
	leaderFor := func(epoch int64) string { return "leader-" + string(rune('a'+int(epoch%26))) }

	var wg sync.WaitGroup
	for i := 1; i <= adopts; i++ {
		wg.Add(1)
		go func(epoch int64) {
			defer wg.Done()
			s.adoptConvergenceMetadata(epoch, leaderFor(epoch), false)
		}(int64(i))
	}

	// Readers run against the same writes, both through the accessor and through
	// the narrower GetClusterEpoch/GetLeaderID the rest of the server uses.
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 400; n++ {
				epoch, leader := s.convergenceMetadata()
				if epoch != 0 && leader != leaderFor(epoch) {
					t.Errorf("epoch %d observed with leader %q, want %q — the pair straddled an adopt",
						epoch, leader, leaderFor(epoch))
					return
				}
				_ = s.GetClusterEpoch()
				_ = s.GetLeaderID()
			}
		}()
	}
	wg.Wait()

	// Whichever adopts won, the epoch must have advanced to the highest offered
	// and must never exceed it.
	if epoch, _ := s.convergenceMetadata(); epoch != adopts {
		t.Errorf("final epoch = %d, want %d (the highest adopt offered)", epoch, adopts)
	}
}

// The reason the split had to be a split rather than just calling the exported
// one: ConfigSync's full-config branch holds the server lock, and the lock is
// not reentrant, so an adopt that retook it would wedge that RPC forever while
// holding the lock every other operation on this daemon needs.
//
// Asserted on a timeout, because a deadlock hangs the package rather than
// failing it (the shape #56's test uses for the same reason).
func TestAdoptingUnderTheServerLockDoesNotWedge(t *testing.T) {
	s, _ := newConfigSyncTestServer(t, "node-local", "node-peer")

	done := make(chan bool, 1)
	go func() {
		s.Lock()
		adopted := s.adoptConvergenceMetadataLocked(7, "leader-7", false)
		s.Unlock()
		done <- adopted
	}()

	select {
	case adopted := <-done:
		if !adopted {
			t.Fatal("epoch 7 should have been adopted from 0")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adopting under the server lock did not return — it retook a non-reentrant lock")
	}

	if epoch, leader := s.convergenceMetadata(); epoch != 7 || leader != "leader-7" {
		t.Errorf("epoch=%d leader=%q, want 7 and leader-7", epoch, leader)
	}
}

// ConfigSync's full-config branch adopts the incoming epoch and its leader, and
// applies the same rule the envelope-only branch does.
//
// This is the test that should have existed before the branch was refactored,
// and did not. Flipping its atLeast argument from false to true — which changes
// which syncs are allowed to install a new leader — passed the entire
// internal/server suite. The branch had no coverage at all: the hand-copied
// compare-and-write it used to carry was correct, and nothing would have said so
// if it had stopped being.
//
// Case two is the one that matters. An equal epoch must NOT install a different
// leader, because that is #2's rule: an equal-epoch peer view does not get to
// override what this node already believes, and a real decision always arrives
// at a higher epoch.
func TestConfigSyncFullConfigAdoptsTheEpochAndItsLeader(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, _ := newConfigSyncTestServer(t, localID, peerID)

	states := map[string]membership.MemberStatus{
		localID: membership.StatusActive,
		peerID:  membership.StatusActive,
	}

	// The config stamp has to advance every time or the branch is not entered:
	// a payload that is not newer is rejected before it reaches the adopt.
	sync := func(t *testing.T, stampVersion int64, epoch int64, leaderID string) {
		t.Helper()
		cfg := peerConfigWithGroup(s, "group1", 2)
		payload, err := buildFullConfigPayload(cfg, states, epoch, leaderID, peerID,
			configStamp{version: stampVersion, origin: peerID})
		if err != nil {
			t.Fatalf("buildFullConfigPayload: %v", err)
		}
		if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
			t.Fatalf("ConfigSync(stamp %d, epoch %d): %v", stampVersion, epoch, err)
		}
	}

	assertConvergence := func(t *testing.T, wantEpoch int64, wantLeader, why string) {
		t.Helper()
		epoch, leader := s.convergenceMetadata()
		if epoch != wantEpoch || leader != wantLeader {
			t.Errorf("%s: epoch=%d leader=%q, want %d and %q", why, epoch, leader, wantEpoch, wantLeader)
		}
	}

	sync(t, 1, 5, "leader-five")
	assertConvergence(t, 5, "leader-five", "a strictly newer epoch must be adopted")

	sync(t, 2, 5, "leader-five-impostor")
	assertConvergence(t, 5, "leader-five",
		"an equal epoch must not install a different leader (docs/TEST-PLAN.md #2)")

	sync(t, 3, 4, "leader-four")
	assertConvergence(t, 5, "leader-five", "an older epoch must be refused")

	sync(t, 4, 9, "leader-nine")
	assertConvergence(t, 9, "leader-nine", "a later strictly newer epoch must still be adopted")
}

// The epoch and the elected node are one decision, and twelve broadcast sites
// used to read them separately: the epoch through its locking accessor, then
// s.leaderID bare in the same expression. broadcastNextEpoch reads the pair
// under one acquisition instead.
//
// It matters more than a race-detector complaint because BroadcastClusterState
// assigns s.leaderID unconditionally -- the `epoch > s.clusterEpoch` check
// guards only the epoch -- so a stale leader read in that window is installed
// and broadcast with nothing standing in its way.
func TestBroadcastNextEpochAdvancesTheEpochUnderTheCurrentElectedNode(t *testing.T) {
	// No peers: the broadcast then has nobody to dial, so this exercises the
	// local epoch and elected-node bookkeeping without waiting on gRPC.
	s, _ := newConfigSyncTestServer(t, "node-local")

	if !s.adoptConvergenceMetadata(4, "leader-four", false) {
		t.Fatal("expected epoch 4 to be adopted")
	}

	_ = s.broadcastNextEpoch(map[string]membership.MemberStatus{
		"node-local": membership.StatusActive,
	})

	epoch, leader := s.convergenceMetadata()
	if epoch != 5 {
		t.Errorf("epoch = %d, want 5 (one past the epoch it read)", epoch)
	}
	if leader != "leader-four" {
		t.Errorf("leader = %q, want leader-four — the broadcast must carry the "+
			"elected node it read, not replace it", leader)
	}
}

// TestBroadcastNextEpochDoesNotRaceAnAdopt is the test the old idiom fails.
//
// s.leaderID is written by adoptConvergenceMetadata under the write lock and was
// read bare by every broadcast site, so -race has both sides here. Run under
// -race this is the whole point; run without it, it still exercises the paths
// concurrently.
func TestBroadcastNextEpochDoesNotRaceAnAdopt(t *testing.T) {
	s, _ := newConfigSyncTestServer(t, "node-local")

	states := map[string]membership.MemberStatus{
		"node-local": membership.StatusActive,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 200; i++ {
			s.adoptConvergenceMetadata(int64(i), "leader-"+string(rune('a'+i%26)), true)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = s.broadcastNextEpoch(states)
		}
	}()
	wg.Wait()

	// The epoch must never have gone backwards, whichever writer won.
	if epoch, _ := s.convergenceMetadata(); epoch < 200 {
		t.Errorf("final epoch = %d, want at least 200", epoch)
	}
}
