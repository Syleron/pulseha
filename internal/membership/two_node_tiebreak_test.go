package membership

import "testing"

// END-2325 fixed caller-dependent tie-breaking. END-2694 replaces that local
// decision entirely: neither ID order nor a missing voter authorizes promotion.
func TestDegradedCandidatesCannotWinWithoutVotes(t *testing.T) {
	for _, asker := range []string{"a", "b"} {
		for _, subject := range []string{"a", "b", "c"} {
			a := newAATestMember("a", "a", StatusPassive, nil)
			b := newAATestMember("b", "b", StatusPassive, nil)
			c := newAATestMember("c", "c", StatusUnknown, nil)
			h, _ := newAPTestChecker(asker, a, b, c)
			if h.initiateNodeStatusVote(subject, StatusActive) {
				t.Fatalf("%s authorized %s without votes", asker, subject)
			}
			if !h.initiateNodeStatusVote(subject, StatusUnknown) {
				t.Fatal("demotion must remain allowed")
			}
		}
	}
}
