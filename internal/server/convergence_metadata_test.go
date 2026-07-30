package server

import (
	"sync"
	"testing"
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
