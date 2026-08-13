package server

import (
	"context"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
)

// serverWithGroupOfSize builds a Server whose config carries n floating IPs, so
// the demotion deadline derived from it can be asserted against the group size
// the way the whitecrane topology sizes it.
func serverWithGroupOfSize(t *testing.T, n int) *Server {
	t.Helper()

	s := newPropagationTestServer(t)
	s.Lock()
	s.config.Groups = map[string][]string{"group1": ipRange(n)}
	s.Unlock()
	return s
}

// Regression for the demotion clamp: MakePassive's remote branch bounded the
// forwarded call at a flat 5s.
//
// context.WithTimeout takes the *sooner* of the parent deadline and the one it is
// given, so the flat value clamped every demotion that crosses this hop — and it
// is the hop that matters, since the remote node is the one that has to release
// and verify the addresses. enforceSingleActive sizes makePassiveTimeout() up to
// 120s and performPromotionAsync sizes its step-1 demotion the same way; both were
// cut back to 5s here. The constants above DemotionTimeoutFor record that even a
// flat 10s was overrun by a loaded incumbent on the 201-address topology, and that
// an overrun is not neutral: DeadlineExceeded is read as "the peer may still own
// its IPs", so a too-short deadline aborts promotions and consolidations that were
// safe. confirmPeerReleasedIPs only ever got the sized deadline because it
// bypasses Server.MakePassive entirely.
func TestRemoteDemotionDeadlineIsSizedToTheGroupNotFlat(t *testing.T) {
	const clamp = 5 * time.Second

	s := serverWithGroupOfSize(t, 201)

	ctx, cancel := s.remoteDemotionContext(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the forwarded demotion carries no deadline at all")
	}

	got := time.Until(deadline)
	want := membership.DemotionTimeoutFor(201)
	if got <= clamp {
		t.Errorf("201 addresses got %s, which is no better than the flat %s it replaced; "+
			"the node that actually releases the addresses is the one behind this hop", got, clamp)
	}
	// Sized from the same helper every other demotion deadline uses, rather than
	// merely being larger than the clamp.
	if drift := want - got; drift < 0 || drift > time.Second {
		t.Errorf("deadline in %s, want ~%s (membership.DemotionTimeoutFor)", got, want)
	}
}

// The derivation from the caller's context has to survive the sizing: a caller
// that already bounded the operation keeps its bound, so an unresponsive peer
// cannot hold the consolidation invariant open for the full group-sized wait.
func TestRemoteDemotionDeadlineNeverOutlivesTheCaller(t *testing.T) {
	s := serverWithGroupOfSize(t, 201)

	parent, cancelParent := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelParent()

	ctx, cancel := s.remoteDemotionContext(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the forwarded demotion carries no deadline at all")
	}
	if got := time.Until(deadline); got > time.Second {
		t.Errorf("deadline in %s, want the caller's 300ms: a sized bound must not "+
			"extend a caller that deliberately bounded the operation more tightly", got)
	}
}

// A node with no floating IPs configured still gets a usable deadline rather than
// one already expired — the group can legitimately be empty during a join.
func TestRemoteDemotionDeadlineHasAFloor(t *testing.T) {
	s := serverWithGroupOfSize(t, 0)

	ctx, cancel := s.remoteDemotionContext(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the forwarded demotion carries no deadline at all")
	}
	if got := time.Until(deadline); got <= 0 {
		t.Errorf("empty group got %s, want a usable deadline", got)
	}
}
