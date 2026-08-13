package membership

import (
	"testing"
	"time"
)

// The deadline has to grow with the group. MakePassive drops and verifies every
// configured floating address on the target, so the flat 10s that preceded this
// was sized for a demotion that only released a node's recorded assignments. On
// the 201-address topology this PR tests against, a healthy but loaded incumbent
// overran it — and an overrun is read as "still owns its IPs", which aborts a
// promotion that was safe.
func TestDemotionTimeoutForSizesToTheGroup(t *testing.T) {
	small := DemotionTimeoutFor(3)
	large := DemotionTimeoutFor(201)

	if large <= small {
		t.Errorf("201 addresses got %s, 3 addresses got %s; the deadline must grow with the group",
			large, small)
	}

	// The concrete case the review named: the old flat deadline was 10s, and the
	// release of 201 addresses has to be given more than that.
	if large <= 10*time.Second {
		t.Errorf("201 addresses got %s, which is no better than the flat 10s it replaced", large)
	}
}

func TestDemotionTimeoutForIsBounded(t *testing.T) {
	// A floor, so a tiny or not-yet-synced group still gets a usable deadline.
	if got := DemotionTimeoutFor(0); got < demoteBaseTimeout {
		t.Errorf("empty group got %s, want at least the %s base", got, demoteBaseTimeout)
	}
	// A negative count is nonsense but must not produce a deadline in the past.
	if got := DemotionTimeoutFor(-5); got < demoteBaseTimeout {
		t.Errorf("negative count got %s, want at least the %s base", got, demoteBaseTimeout)
	}
	// And a ceiling, so a huge or misconfigured group cannot make the wait
	// effectively unbounded — DeadlineExceeded is what keeps promotion safe.
	if got := DemotionTimeoutFor(1_000_000); got != demoteMaxTimeout {
		t.Errorf("a million addresses got %s, want the %s cap", got, demoteMaxTimeout)
	}
}

// The consolidation invariant must use the sized deadline too: it issues the same
// MakePassive, so a flat deadline mis-sizes it in exactly the same way.
func TestConsolidationDemotionDeadlineIsSizedToTheGroup(t *testing.T) {
	a := newAATestMember("node-a", "host-a", StatusActive, nil)
	h, _ := newAPTestChecker("node-a", a)

	// newAPTestChecker configures a three-address group.
	if got, want := h.makePassiveTimeout(), DemotionTimeoutFor(3); got != want {
		t.Errorf("consolidation deadline = %s, want %s for a 3-address group", got, want)
	}

	h.members.config.Groups["group2"] = ipRangeForTest(198)
	if got, want := h.makePassiveTimeout(), DemotionTimeoutFor(201); got != want {
		t.Errorf("consolidation deadline = %s, want %s once the group holds 201 addresses", got, want)
	}
}

// ipRangeForTest builds n distinct addresses.
func ipRangeForTest(n int) []string {
	ips := make([]string, n)
	for i := 0; i < n; i++ {
		ips[i] = "10.201." + itoaForTest(i/254) + "." + itoaForTest(i%254+1) + "/23"
	}
	return ips
}

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
