package server

import (
	"testing"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

// Standby exists because Active answered two questions at once — "is this
// daemon healthy and eligible" and "is it serving floating IPs" — and gave the
// same answer to both. A node that has been promoted but assigned nothing was
// reported as Active, which told an operator nothing about whether traffic was
// reaching it.
//
// The load-bearing case here is hasAssignmentTruth. An empty assignment list is
// only evidence of holding nothing where the list is knowledge; on a peer in
// active-passive it means "this node does not know", and calling that Standby
// would be worse than the vague Active it replaced.
func TestDeriveMemberStatus(t *testing.T) {
	tests := []struct {
		name               string
		status             membership.MemberStatus
		assignedIPs        int
		hasAssignmentTruth bool
		want               rpc.MemberStatusEnum
	}{
		{
			name:               "active holding nothing, list trustworthy, is Standby",
			status:             membership.StatusActive,
			assignedIPs:        0,
			hasAssignmentTruth: true,
			want:               rpc.MemberStatusEnum_MEMBER_STATUS_STANDBY,
		},
		{
			name:               "active holding addresses stays Active",
			status:             membership.StatusActive,
			assignedIPs:        61,
			hasAssignmentTruth: true,
			want:               rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE,
		},
		{
			// The #1/#21 shape: an election-promoted node in active-passive
			// holds every group address while its ActiveIPs is still empty, and
			// peers get no self-report in that mode. Reporting Standby here
			// would claim the node serving all the traffic is serving none.
			name:               "active holding nothing without trustworthy list stays Active",
			status:             membership.StatusActive,
			assignedIPs:        0,
			hasAssignmentTruth: false,
			want:               rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE,
		},
		{
			// Passive already means "holds nothing" in active-passive, so it is
			// left alone: renaming it would churn the wire value every existing
			// deployment reports without telling an operator anything new.
			name:               "passive is unchanged even with a trustworthy empty list",
			status:             membership.StatusPassive,
			assignedIPs:        0,
			hasAssignmentTruth: true,
			want:               rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE,
		},
		{
			// Maintenance is a deliberate operator decision to exclude the node
			// from promotion. Standby means "eligible", so it must not swallow
			// Maintenance just because the node holds nothing.
			name:               "maintenance outranks an empty assignment list",
			status:             membership.StatusMaintenance,
			assignedIPs:        0,
			hasAssignmentTruth: true,
			want:               rpc.MemberStatusEnum_MEMBER_STATUS_MAINTENANCE,
		},
		{
			name:               "unknown is a health fact and is never derived away",
			status:             membership.StatusUnknown,
			assignedIPs:        0,
			hasAssignmentTruth: true,
			want:               rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveMemberStatus(tt.status, tt.assignedIPs, tt.hasAssignmentTruth)
			if got != tt.want {
				t.Errorf("deriveMemberStatus(%v, %d, %t) = %v, want %v",
					membership.StatusToString(tt.status), tt.assignedIPs, tt.hasAssignmentTruth, got, tt.want)
			}
		})
	}
}

// Tenancy must stay derived. A stored Standby would reintroduce exactly the
// defect it is meant to describe, so this pins that the display value never
// leaks into the internal enum that placement and demotion read.
func TestStandbyIsNotAStoredMemberStatus(t *testing.T) {
	for _, s := range []membership.MemberStatus{
		membership.StatusUnknown,
		membership.StatusActive,
		membership.StatusPassive,
		membership.StatusMaintenance,
	} {
		if membership.StatusToString(s) == "Standby" {
			t.Fatalf("Standby must be derived at the status boundary, not stored as MemberStatus(%d)", s)
		}
	}
}
