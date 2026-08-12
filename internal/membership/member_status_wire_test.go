package membership

import (
	"testing"

	"github.com/syleron/pulseha/rpc"
)

// TestMemberStatusOrdinalsMatchTheProto pins the Go ordinals to the proto enum,
// because the ordinal *is* the wire format.
//
// `member_states` is encoded as `int(MemberStatus)` in both broadcast paths
// (buildFullConfigPayload and BroadcastClusterState) and decoded straight back
// into a MemberStatus with no range validation, so the iota block in member.go is
// a wire contract even though it does not look like one.
//
// Dropping StatusPartialActive from that block shifted StatusMaintenance from 4
// to 3 while the proto did the right thing — `reserved 3`, MAINTENANCE = 4 — and
// the two disagreeing breaks a rolling upgrade in both directions. New to old:
// Maintenance(3) is read as the PartialActive this PR removes. Old to new:
// Maintenance(4) becomes an undefined MemberStatus that matches no arm of
// redistributeOrphanedIPs' switch, so the node's ActiveIPs are neither counted as
// hosted nor cleared — its addresses look orphaned while its own record still
// claims them, and the coordinator redistributes addresses it may still hold.
//
// A gap at 3 costs nothing: nothing indexes an array or slice by MemberStatus.
func TestMemberStatusOrdinalsMatchTheProto(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  MemberStatus
		want rpc.MemberStatusEnum
	}{
		{"Unknown", StatusUnknown, rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN},
		{"Active", StatusActive, rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE},
		{"Passive", StatusPassive, rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE},
		{"Maintenance", StatusMaintenance, rpc.MemberStatusEnum_MEMBER_STATUS_MAINTENANCE},
	} {
		if int(tc.got) != int(tc.want) {
			t.Errorf("membership.Status%s = %d, but the proto sends it as %d (%s); "+
				"member_states carries the raw Go ordinal, so these must not diverge",
				tc.name, int(tc.got), int(tc.want), tc.want)
		}
	}
}

// TestMemberStatusThreeIsUnusedSoTheProtoReservationHolds asserts the gap the
// proto's `reserved 3` describes is actually a gap on this side too. Reusing 3
// for a future status would put a value on the wire that an older binary reads as
// StatusPartialActive, which is the failure the reservation exists to prevent.
func TestMemberStatusThreeIsUnusedSoTheProtoReservationHolds(t *testing.T) {
	const reservedPartialActive = 3
	for _, st := range []MemberStatus{StatusUnknown, StatusActive, StatusPassive, StatusMaintenance} {
		if int(st) == reservedPartialActive {
			t.Errorf("%s occupies ordinal 3, which the proto reserves for the removed "+
				"PARTIAL_ACTIVE; an older peer would decode it as that status",
				StatusToString(st))
		}
	}
}
