package server

import (
	"encoding/json"
	"io"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
)

// What a node says about the addresses it holds has to distinguish "I hold
// nothing" from "I am not reporting", because ConfigSync gates on exactly that:
// `senderActiveIPs != nil`.
//
// An empty slice marshals to `[]` and clears the peer's record. nil marshals to
// `null` and leaves it alone. So a node that had released every address and
// reported nil would go quiet about it, and its peers would keep counting those
// addresses as hosted — defect #58's shape, reached by a different road.
//
// This test exists because converting the site onto Member.GetActiveIPs
// reintroduced exactly that: GetActiveIPs returns nil for an empty set, the
// whole server suite still passed, and only reading the receiver's nil check
// caught it.
func TestSelfReportedAddressesDistinguishesEmptyFromAbsent(t *testing.T) {
	newMember := func(ips []string) *membership.Member {
		m := membership.NewMember("node-a", "host-a", nil, log.New(io.Discard))
		m.SetActiveIPs(ips)
		return m
	}

	t.Run("holding nothing reports an empty list, not absence", func(t *testing.T) {
		got := selfReportedAddresses(newMember(nil))
		if got == nil {
			t.Fatal("reported nil, which ConfigSync reads as 'no report' — a node " +
				"that released everything must say so")
		}
		if len(got) != 0 {
			t.Fatalf("reported %v, want an empty list", got)
		}
		assertMarshalsTo(t, got, `{"sender_active_ips":[]}`)
	})

	t.Run("holding addresses reports them", func(t *testing.T) {
		got := selfReportedAddresses(newMember([]string{"10.0.0.1/24", "10.0.0.2/24"}))
		if len(got) != 2 {
			t.Fatalf("reported %v, want two addresses", got)
		}
	})

	t.Run("nil would marshal as absence, which is the trap", func(t *testing.T) {
		// The negative control: the thing the helper exists to avoid.
		assertMarshalsTo(t, nil, `{"sender_active_ips":null}`)
	})
}

func assertMarshalsTo(t *testing.T, ips []string, want string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"sender_active_ips": ips})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(b) != want {
		t.Errorf("marshalled to %s, want %s", b, want)
	}
}
