package client

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/syleron/pulseha/rpc"
)

// capture runs f with stdout redirected, and returns what it printed.
func capture(t *testing.T, f func()) string {
	t.Helper()

	prev := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = prev

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	return buf.String()
}

// Regression for docs/TEST-PLAN.md defect #115. A join parks the node on purpose
// and said so nowhere, so `cluster join` reported success and `pulsectl status`
// then reported the cluster degraded, with nothing connecting the two.
func TestAParkedNodeIsExplainedAfterJoining(t *testing.T) {
	const id = "128239be-2bb1-4a34-b85f-5050ae3e31ca"

	out := capture(t, func() {
		printJoinedInMaintenance(&rpc.Member{
			NodeId: id,
			Status: rpc.MemberStatusEnum_MEMBER_STATUS_MAINTENANCE,
		})
	})

	for _, want := range []string{
		"maintenance mode",
		"degraded",              // the word the operator is about to read in status
		"not a failure",         // and what it means
		"group assign",          // step 1
		"maintenance --disable", // step 2
		id,                      // both commands are copy-pasteable
	} {
		if !strings.Contains(out, want) {
			t.Errorf("join output does not mention %q:\n%s", want, out)
		}
	}
}

// The message is driven off what the member reports, not off an assumption that
// joins park nodes. A node that comes back Passive must produce nothing, or the
// default changing turns this into a confident lie.
func TestNothingIsSaidWhenTheNodeIsNotParked(t *testing.T) {
	for _, status := range []rpc.MemberStatusEnum{
		rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE,
		rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE,
		rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN,
	} {
		out := capture(t, func() {
			printJoinedInMaintenance(&rpc.Member{NodeId: "n1", Status: status})
		})
		if out != "" {
			t.Errorf("status %v printed maintenance guidance:\n%s", status, out)
		}
	}
}

func TestANilMemberSaysNothing(t *testing.T) {
	if out := capture(t, func() { printJoinedInMaintenance(nil) }); out != "" {
		t.Errorf("nil member printed %q", out)
	}
}
