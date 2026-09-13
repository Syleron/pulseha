package server

import (
	"strings"
	"testing"
)

// Regression for docs/TEST-PLAN.md defect #113.
//
// The join handler logged `Expected token: %s, Received token: %s` at debug, and
// debug is exactly what an operator turns on when a join will not authenticate --
// so the one situation that produced this line was the one where the cluster's
// shared secret went to the journal, and through syslog to wherever that is
// forwarded.
func TestTokenFingerprintDoesNotLeakTheToken(t *testing.T) {
	const token = "8f14e45f-ceea-467a-9f2d-1c2b3a4d5e6f"

	fp := tokenFingerprint(token)
	if strings.Contains(fp, token) || strings.Contains(token, fp) {
		t.Fatalf("fingerprint %q carries the token %q", fp, token)
	}
	if len(fp) != 12 {
		t.Errorf("fingerprint = %q (%d chars), want 12: long enough to be unique, "+
			"short enough for an operator to compare by eye", fp, len(fp))
	}
}

// It still has to answer the question the old line was there for: are these the
// same token, and if not, which side changed.
func TestTokenFingerprintDistinguishesTokens(t *testing.T) {
	a := tokenFingerprint("8f14e45f-ceea-467a-9f2d-1c2b3a4d5e6f")
	b := tokenFingerprint("1c2b3a4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d")

	if a == b {
		t.Fatal("two different tokens share a fingerprint; the log cannot tell them apart")
	}
	if a != tokenFingerprint("8f14e45f-ceea-467a-9f2d-1c2b3a4d5e6f") {
		t.Error("fingerprint is not stable; two nodes could not compare theirs")
	}
}

// "The joiner sent nothing" and "the joiner sent the wrong thing" are different
// problems, and telling them apart is why the operator is reading this line.
func TestEmptyTokenIsNamedRatherThanHashed(t *testing.T) {
	if got := tokenFingerprint(""); got != "<empty>" {
		t.Errorf("tokenFingerprint(\"\") = %q, want <empty>", got)
	}
	if got := tokenFingerprint("   \n"); got != "<empty>" {
		t.Errorf("whitespace-only token = %q, want <empty>", got)
	}
}

func TestTokensEqual(t *testing.T) {
	const token = "8f14e45f-ceea-467a-9f2d-1c2b3a4d5e6f"

	t.Run("a matching token is accepted", func(t *testing.T) {
		if !tokensEqual(token, token) {
			t.Error("the right token was rejected")
		}
	})

	t.Run("surrounding whitespace is forgiven", func(t *testing.T) {
		// Operators move this string by hand. A trailing newline is not a wrong
		// token, it is a right token that arrived with punctuation.
		if !tokensEqual("  "+token+"\n", token) {
			t.Error("a pasted token with surrounding whitespace was rejected")
		}
	})

	t.Run("case is not folded", func(t *testing.T) {
		// PR #201 proposed strings.EqualFold here. Folding case throws away
		// entropy from a secret to save an operator from a mistake they have not
		// been observed making.
		if tokensEqual(strings.ToUpper(token), token) {
			t.Error("case-folded match accepted; that halves the alphabet of a secret")
		}
	})

	t.Run("a Bearer prefix is not stripped", func(t *testing.T) {
		// Also #201's. This is not an HTTP authorization header, and inventing
		// accepted spellings for a secret widens what counts as valid.
		if tokensEqual("Bearer "+token, token) {
			t.Error("a Bearer-prefixed token was accepted")
		}
	})

	t.Run("an empty token never matches a configured one", func(t *testing.T) {
		if tokensEqual("", token) {
			t.Error("an empty token was accepted")
		}
	})

	t.Run("a wrong token is rejected", func(t *testing.T) {
		if tokensEqual("1c2b3a4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", token) {
			t.Error("a different token was accepted")
		}
	})

	t.Run("a prefix of the token is rejected", func(t *testing.T) {
		// The case a length-leaking or early-exit comparison is most likely to
		// get wrong.
		if tokensEqual(token[:10], token) {
			t.Error("a prefix of the token was accepted")
		}
	})
}
