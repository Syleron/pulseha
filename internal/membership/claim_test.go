package membership

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Claim is a member's status together with the Floating IPs it says it holds.
// The pair exists because every defect in the #2/#26/#58 family came from one
// half being written without the other, and because internal/server was reaching
// across the package boundary to write both by hand at twenty sites.

func TestClaimReturnsBothFieldsTogether(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24", "10.0.0.2/24"})

	c := m.Claim()
	if c.Status != StatusActive {
		t.Errorf("Status = %s, want Active", StatusToString(c.Status))
	}
	if len(c.ActiveIPs) != 2 {
		t.Errorf("ActiveIPs = %v, want two addresses", c.ActiveIPs)
	}
}

// TestClaimDoesNotAliasTheMembersSlice matters because a Claim crosses the
// package boundary. If the read handed out the member's own slice, a caller in
// internal/server could mutate the member's record with no lock held at all —
// which is the class of bug this API exists to close, reintroduced through its
// own front door.
func TestClaimDoesNotAliasTheMembersSlice(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})

	c := m.Claim()
	c.ActiveIPs[0] = "192.0.2.99/24"

	if got := m.Claim().ActiveIPs[0]; got != "10.0.0.1/24" {
		t.Errorf("mutating the returned claim changed the member: %s", got)
	}
}

// TestSetClaimDoesNotAliasTheCallersSlice pins a deliberate behaviour change.
//
// MakeActive used to assign the caller's slice straight into m.ActiveIPs, so a
// caller reusing its buffer mutated the member's record from outside the lock.
// Nothing depended on it and nothing should be able to.
func TestSetClaimDoesNotAliasTheCallersSlice(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusPassive, nil)

	ips := []string{"10.0.0.1/24"}
	m.SetClaim(Claim{Status: StatusActive, ActiveIPs: ips})
	ips[0] = "192.0.2.99/24"

	if got := m.Claim().ActiveIPs[0]; got != "10.0.0.1/24" {
		t.Errorf("mutating the caller's slice changed the member: %s", got)
	}
}

func TestSetClaimWithNoAddressesClearsThem(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})

	m.SetClaim(Claim{Status: StatusPassive})

	c := m.Claim()
	if c.Status != StatusPassive {
		t.Errorf("Status = %s, want Passive", StatusToString(c.Status))
	}
	if len(c.ActiveIPs) != 0 {
		t.Errorf("ActiveIPs = %v, want cleared", c.ActiveIPs)
	}
}

func TestUpdateClaimAppliesWhatTheDecisionReturns(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusPassive, []string{"10.0.0.1/24"})

	applied := m.UpdateClaim(func(current Claim) (Claim, bool) {
		if current.Status != StatusPassive {
			t.Errorf("the decision saw Status = %s, want Passive", StatusToString(current.Status))
		}
		if len(current.ActiveIPs) != 1 {
			t.Errorf("the decision saw ActiveIPs = %v, want one address", current.ActiveIPs)
		}
		return Claim{Status: StatusActive, ActiveIPs: []string{"10.0.0.2/24"}}, true
	})

	if !applied {
		t.Error("UpdateClaim reported it did not write, but the decision returned true")
	}
	c := m.Claim()
	if c.Status != StatusActive || len(c.ActiveIPs) != 1 || c.ActiveIPs[0] != "10.0.0.2/24" {
		t.Errorf("claim = %+v, want Active with 10.0.0.2/24", c)
	}
}

// TestUpdateClaimLeavesTheMemberAloneWhenTheDecisionDeclines is the arm
// ConfigSync depends on: a peer's equal-epoch view of the local node's status is
// ignored outright, and "ignored" has to mean nothing was written.
func TestUpdateClaimLeavesTheMemberAloneWhenTheDecisionDeclines(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})

	applied := m.UpdateClaim(func(Claim) (Claim, bool) {
		return Claim{Status: StatusUnknown}, false
	})

	if applied {
		t.Error("UpdateClaim reported a write, but the decision returned false")
	}
	c := m.Claim()
	if c.Status != StatusActive || len(c.ActiveIPs) != 1 {
		t.Errorf("claim = %+v, want the original Active with one address", c)
	}
}

// TestUpdateClaimIsOneCriticalSection is the reason the method exists at all
// rather than a read followed by a write.
//
// The decision runs with the lock held, so nothing can move the member between
// the read it is given and the write it produces. Asserted by having the
// decision block until a concurrent writer has definitely tried and been made
// to wait.
func TestUpdateClaimIsOneCriticalSection(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusPassive, nil)

	inside := make(chan struct{})
	release := make(chan struct{})
	writerDone := make(chan struct{})

	go func() {
		m.UpdateClaim(func(current Claim) (Claim, bool) {
			close(inside)
			<-release
			// Whatever the other goroutine wanted, it cannot have landed yet.
			if current.Status != StatusPassive {
				t.Errorf("the decision's view changed under it: %s",
					StatusToString(current.Status))
			}
			current.Status = StatusActive
			return current, true
		})
	}()

	<-inside
	go func() {
		defer close(writerDone)
		m.SetClaim(Claim{Status: StatusMaintenance})
	}()

	// The competing writer must still be blocked on the lock.
	select {
	case <-writerDone:
		t.Fatal("a concurrent SetClaim completed while UpdateClaim held the lock")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	<-writerDone

	// The last writer wins, which is ordinary mutual exclusion; the point is
	// that it could not interleave.
	if got := m.Claim().Status; got != StatusMaintenance {
		t.Errorf("final status = %s, want Maintenance (the later writer)", StatusToString(got))
	}
}

// TestUpdateClaimCallingBackIntoTheMemberIsCaught is the test that justifies
// offering UpdateClaim at all.
//
// The decision runs under the member lock, which is exactly #85's shape: a
// server-authored closure that reaches back into a locking Member method would
// wedge the member lock forever, in silence. That risk is acceptable only
// because it is now loud — pulselock panics on the re-acquisition under test
// rather than hanging (docs/adr/0003-instrumented-mutexes.md).
//
// If this test ever stops panicking, UpdateClaim has become unsafe to hand to
// another package and should be withdrawn.
func TestUpdateClaimCallingBackIntoTheMemberIsCaught(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusPassive, nil)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		m.UpdateClaim(func(current Claim) (Claim, bool) {
			// The mistake: a locking sibling, from inside the locked region.
			_ = m.GetStatus()
			return current, true
		})
	}()

	if recovered == nil {
		t.Fatal("a locking call from inside the decision did not panic — " +
			"UpdateClaim is only safe to expose while this is caught")
	}
	msg, ok := recovered.(string)
	if !ok || !strings.Contains(msg, "pulselock") {
		t.Fatalf("panicked with something other than a pulselock report: %v", recovered)
	}
}

func TestSetActiveIPsLeavesTheStatusAlone(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusMaintenance, nil)

	m.SetActiveIPs([]string{"10.0.0.1/24"})

	c := m.Claim()
	if c.Status != StatusMaintenance {
		t.Errorf("Status = %s, want Maintenance untouched", StatusToString(c.Status))
	}
	if len(c.ActiveIPs) != 1 {
		t.Errorf("ActiveIPs = %v, want one address", c.ActiveIPs)
	}
}

func TestSetActiveIPsDoesNotAliasTheCallersSlice(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusActive, nil)

	ips := []string{"10.0.0.1/24"}
	m.SetActiveIPs(ips)
	ips[0] = "192.0.2.99/24"

	if got := m.GetActiveIPs()[0]; got != "10.0.0.1/24" {
		t.Errorf("mutating the caller's slice changed the member: %s", got)
	}
}

func TestSetCapacityIsNotPartOfTheClaim(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})

	m.SetCapacity(7)

	m.mu.Lock()
	capacity := m.Capacity
	m.mu.Unlock()
	if capacity != 7 {
		t.Errorf("Capacity = %d, want 7", capacity)
	}
	// The claim must be untouched: capacity is a configured limit, not something
	// the member asserts about its own state.
	c := m.Claim()
	if c.Status != StatusActive || len(c.ActiveIPs) != 1 {
		t.Errorf("SetCapacity changed the claim: %+v", c)
	}
}

// TestMarkUnreachableIsANamedClaim keeps the operation's meaning after it was
// reimplemented on SetClaim. Its comment is the invariant: a reader seeing
// Unknown against the old ActiveIPs would conclude a down node still owns the
// group.
func TestMarkUnreachableIsANamedClaim(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24", "10.0.0.2/24"})

	m.MarkUnreachable()

	c := m.Claim()
	if c.Status != StatusUnknown {
		t.Errorf("Status = %s, want Unknown", StatusToString(c.Status))
	}
	if len(c.ActiveIPs) != 0 {
		t.Errorf("ActiveIPs = %v, want cleared — a down node must not still claim addresses", c.ActiveIPs)
	}
}

// TestConcurrentClaimReadsNeverSeeAMismatchedPair is the invariant the type
// exists for, given -race both sides to pair up. Every observed combination must
// be one that was actually written together.
func TestConcurrentClaimReadsNeverSeeAMismatchedPair(t *testing.T) {
	m := newAATestMember("node-a", "host-a", StatusPassive, nil)

	// Each status is written with an address set that identifies it, so a
	// mismatched pair is detectable rather than merely possible.
	ipsFor := func(s MemberStatus) []string {
		return []string{fmt.Sprintf("10.0.0.%d/24", int(s))}
	}
	statuses := []MemberStatus{StatusActive, StatusPassive, StatusMaintenance, StatusUnknown}

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				s := statuses[(w+i)%len(statuses)]
				m.SetClaim(Claim{Status: s, ActiveIPs: ipsFor(s)})
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				c := m.Claim()
				if len(c.ActiveIPs) == 0 {
					continue // the initial state, before any writer landed
				}
				if want := ipsFor(c.Status); c.ActiveIPs[0] != want[0] {
					t.Errorf("status %s observed with %v, want %v — the pair straddled a write",
						StatusToString(c.Status), c.ActiveIPs, want)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestAddressesNotIn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		held, want []string
		expect     []string
	}{
		{name: "nothing offered", held: []string{"a"}, want: nil, expect: nil},
		{name: "nothing held", held: nil, want: []string{"a", "b"}, expect: []string{"a", "b"}},
		{name: "all already held", held: []string{"a", "b"}, want: []string{"a", "b"}, expect: nil},
		{name: "some already held", held: []string{"a"}, want: []string{"a", "b"}, expect: []string{"b"}},
		{
			// Offering the same new address twice must add it once: the result
			// is what gets announced, and #33's cost was announcing addresses
			// the interface did not hold.
			name: "duplicates within the offer", held: nil,
			want: []string{"a", "a", "b"}, expect: []string{"a", "b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := addressesNotIn(tc.held, tc.want)
			if len(got) != len(tc.expect) {
				t.Fatalf("got %v, want %v", got, tc.expect)
			}
			for i := range got {
				if got[i] != tc.expect[i] {
					t.Fatalf("got %v, want %v (order matters: it is the announce order)", got, tc.expect)
				}
			}
		})
	}
}

// TestMemberDoesNotExposeItsLock pins the un-embedding.
//
// Member's mutex is a named private field, so Lock() is not part of its public
// surface and no other package can take it. That is worth a test because
// re-embedding it is a one-word change that would compile, pass everything else,
// and silently reopen the boundary: internal/server held this lock at twenty
// sites and edited the fields underneath it by hand.
//
// Checked through sync.Locker rather than by reflection over field names,
// because what matters is whether the method set exposes the lock, not what the
// field is called.
func TestMemberDoesNotExposeItsLock(t *testing.T) {
	var m any = &Member{}

	if _, ok := m.(sync.Locker); ok {
		t.Error("*Member satisfies sync.Locker, so its mutex is embedded again — " +
			"callers outside this package can take it, and the claim operations " +
			"stop being the only way in")
	}
	// The claim operations are the intended surface, and they must still be there.
	if _, ok := m.(interface {
		Claim() Claim
		SetClaim(Claim)
		UpdateClaim(func(Claim) (Claim, bool)) bool
	}); !ok {
		t.Error("*Member no longer offers the claim operations")
	}
}
