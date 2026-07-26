package server

import (
	"testing"

	"github.com/syleron/pulseha/internal/membership"
)

// Regression: an incoming ConfigSync that reports the local node as Unknown
// must not be treated as a demotion. On the whitecrane cluster a node holding
// 203 floating IPs was blocked long enough in the per-IP GARP for peers to mark
// it Unknown; it broadcast that back, the node released every IP including the
// live Management VIPs, and took ~13 minutes to bring them all back up.
func TestIsDemotion(t *testing.T) {
	cases := []struct {
		name string
		old  membership.MemberStatus
		new  membership.MemberStatus
		want bool
	}{
		{"Active to Passive is a demotion", membership.StatusActive, membership.StatusPassive, true},
		{"Active to Maintenance is a demotion", membership.StatusActive, membership.StatusMaintenance, true},
		{"Active to Unknown is not a demotion", membership.StatusActive, membership.StatusUnknown, false},
		{"Active to Active is not a demotion", membership.StatusActive, membership.StatusActive, false},
		{"Passive to Passive is not a demotion", membership.StatusPassive, membership.StatusPassive, false},
		{"Unknown to Passive is not a demotion", membership.StatusUnknown, membership.StatusPassive, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDemotion(tc.old, tc.new); got != tc.want {
				t.Errorf("isDemotion(%s, %s) = %v, want %v",
					membership.StatusToString(tc.old), membership.StatusToString(tc.new), got, tc.want)
			}
		})
	}
}
