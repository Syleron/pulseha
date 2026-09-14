// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package clustertls

import (
	"strings"
	"testing"
)

const (
	aSecret = "6f1b0b7e-0c1a-4d2f-9a3b-5c6d7e8f9a0b"
	aPrint  = "9c56cc51b374c3ba189210d5b6d4bf57790d351c96c47c02190ecf1e430635ab"
)

func TestAJoinTokenSurvivesBeingCarriedByHand(t *testing.T) {
	token := FormatJoinToken(aSecret, aPrint)

	// The whole point is that this is one string an operator copies once.
	if strings.ContainsAny(token, " \t\n") {
		t.Errorf("token %q has whitespace in it; it has to survive a copy and paste", token)
	}

	for _, carried := range []string{
		token,
		"  " + token + "\n",
		// The fingerprint uppercased. It is hex, and hex gets retyped in whichever
		// case the thing that displayed it chose. The secret is deliberately not
		// folded with it: it is compared byte for byte at the other end, so
		// "helpfully" normalising it here would break a join rather than rescue one.
		aSecret + "." + strings.ToUpper(aPrint),
	} {
		secret, fingerprint, pinned := ParseJoinToken(carried)
		if !pinned {
			t.Errorf("ParseJoinToken(%q) did not report a pin", carried)
		}
		if secret != aSecret {
			t.Errorf("ParseJoinToken(%q) secret = %q, want %q verbatim", carried, secret, aSecret)
		}
		if fingerprint != aPrint {
			t.Errorf("ParseJoinToken(%q) fingerprint = %q, want %q", carried, fingerprint, aPrint)
		}
	}
}

// A permissive cluster's token names no certificate, and must not look as though
// it does. There is no handshake to pin there, and a pin that cannot be checked
// is a claim the token cannot back.
func TestAPermissiveClustersTokenIsJustTheSecret(t *testing.T) {
	token := FormatJoinToken(aSecret, "")
	if token != aSecret {
		t.Errorf("token = %q, want the bare secret %q", token, aSecret)
	}

	secret, fingerprint, pinned := ParseJoinToken(token)
	if secret != aSecret {
		t.Errorf("secret = %q, want %q", secret, aSecret)
	}
	if pinned || fingerprint != "" {
		t.Errorf("pinned = %v, fingerprint = %q; a token with no pin must not produce one",
			pinned, fingerprint)
	}
}

// A token that is mistyped after the separator comes back as a bad fingerprint
// rather than as no fingerprint at all.
//
// The difference decides what the join does. A bad fingerprint is refused by
// PinnedClientCredentials and the operator is told to check the token; no
// fingerprint is a plaintext join. Turning the first into the second is how an
// operator who asked for a pinned join silently gets an unpinned one.
func TestAMistypedFingerprintIsNotTreatedAsAbsent(t *testing.T) {
	for _, damaged := range []string{
		aSecret + "." + aPrint[:40],
		aSecret + "." + "nonsense",
		aSecret + ".",
	} {
		secret, fingerprint, pinned := ParseJoinToken(damaged)
		if secret != aSecret {
			t.Errorf("ParseJoinToken(%q) secret = %q, want %q", damaged, secret, aSecret)
		}
		if !pinned {
			t.Errorf("ParseJoinToken(%q) reported no pin, so the join would go out in "+
				"clear against a cluster the operator asked to pin", damaged)
		}
		if _, err := PinnedClientCredentials("", fingerprint); err == nil {
			t.Errorf("ParseJoinToken(%q) produced %q, which was accepted as a fingerprint",
				damaged, fingerprint)
		}
	}

	// The one case that is genuinely absent rather than damaged.
	if _, fingerprint, pinned := ParseJoinToken(aSecret); pinned || fingerprint != "" {
		t.Errorf("a bare secret produced pinned = %v, fingerprint %q", pinned, fingerprint)
	}
}
