package server

import (
	"errors"
	"testing"
)

// Regression for TC-6 defect #21. MakePassive used to build its drop set from the node's
// configured IP groups and return Success:true regardless of what actually happened — so a
// node mid-mode-change dropped nothing, reported a successful demotion, and the promoting
// node claimed 201 addresses it was still serving. Release is now confirmed against the
// interfaces, and anything indeterminate counts as still held.
// The real interface lookup is netlink-based and so Linux-only; the decision logic is tested
// here through a stub. What matters is the direction of each verdict.
func TestFilterStillHeld(t *testing.T) {
	present := func(up map[string]bool, failOn string) ipPresenceFunc {
		return func(ip string) (bool, string, error) {
			if ip == failOn {
				return false, "", errors.New("lookup failed")
			}
			return up[ip], "enX0", nil
		}
	}

	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}

	// A fully completed release must report nothing held, or promotion would never proceed.
	if got := filterStillHeld(ips, present(map[string]bool{}, "")); len(got) != 0 {
		t.Errorf("all released: got %v, want empty", got)
	}

	// The TC-6 case: addresses still up must be reported so the promotion is refused.
	up := map[string]bool{"10.0.0.1": true, "10.0.0.3": true}
	got := filterStillHeld(ips, present(up, ""))
	if len(got) != 2 || got[0] != "10.0.0.1" || got[1] != "10.0.0.3" {
		t.Errorf("partial release: got %v, want [10.0.0.1 10.0.0.3]", got)
	}

	// An address we cannot look up counts as held — never assume it is gone.
	if got := filterStillHeld(ips, present(map[string]bool{}, "10.0.0.2")); len(got) != 1 || got[0] != "10.0.0.2" {
		t.Errorf("lookup error: got %v, want [10.0.0.2] treated as held", got)
	}

	if got := filterStillHeld(nil, present(map[string]bool{}, "")); len(got) != 0 {
		t.Errorf("empty input: got %v, want empty", got)
	}
}

// Regression: TC-6 on the whitecrane cluster. Switching to active-active wedged the Active
// node under s.Lock() while it ran serial per-IP GARP. Peers stopped seeing any Active at all,
// node-2 won an election and promoted itself, and because there was no Active *present* to
// demote it logged shouldDemote=false and never sent MakePassive. The wedged node still held
// all 201 addresses, so 103 of them ended up live on two nodes at once for ~5 minutes.
//
// The fix must distinguish three situations that all look like "the Active is unreachable":
//   - the peer is wedged but alive — it definitely still owns its IPs
//   - the peer is gone and we are on the majority side — normal failover, must still work
//   - the peer is gone and we are on the minority side — claiming would split the cluster
//
// peerStillAlive is derived from confirmPeerReleasedIPs, which issues the demotion RPC to the
// peer directly. Do NOT route that through Server.MakePassive: it flattens every remote failure
// into (&Response{Success: false}, nil), so the error is always nil and every peer — wedged,
// refused, or demoted — looks identical. The first cut of this fix did exactly that and the
// guard silently degraded to a no-op that logged "confirmed released" for a stopped daemon.
// Only a transport-level failure proves nothing is holding the addresses; everything
// indeterminate must count as still-alive.
func TestCanPromoteWithoutConfirmedRelease(t *testing.T) {
	cases := []struct {
		name           string
		peerStillAlive bool
		haveQuorum     bool
		forceDemote    bool
		want           bool
	}{
		// The TC-6 case: the peer answered the connection but not the RPC, so it is alive and
		// still holding every floating IP. Quorum is irrelevant — this must never proceed.
		{"wedged peer with quorum must not be claimed", true, true, false, false},
		{"wedged peer without quorum must not be claimed", true, false, false, false},

		// Genuine node death on the majority side. Failover must not regress into a hang.
		{"dead peer with quorum promotes", false, true, false, true},

		// Minority side of a partition must not claim addresses it cannot prove were released.
		{"dead peer without quorum must not be claimed", false, false, false, false},

		// An operator forcing the issue overrides every case above.
		{"force overrides a wedged peer", true, true, true, true},
		{"force overrides a lack of quorum", false, false, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canPromoteWithoutConfirmedRelease(tc.peerStillAlive, tc.haveQuorum, tc.forceDemote)
			if got != tc.want {
				t.Errorf("canPromoteWithoutConfirmedRelease(peerStillAlive=%v, haveQuorum=%v, forceDemote=%v) = %v, want %v",
					tc.peerStillAlive, tc.haveQuorum, tc.forceDemote, got, tc.want)
			}
		})
	}
}
