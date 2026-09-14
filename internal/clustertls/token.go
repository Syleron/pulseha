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

import "strings"

// tokenSeparator divides the two halves of a join token.
//
// A dot because neither half can contain one: the secret is a UUID (hex and
// hyphens) and the fingerprint is hex. So the split is unambiguous in both
// directions and needs no escaping, which matters for a string a human retypes.
const tokenSeparator = "."

// FormatJoinToken renders what an operator carries to a joining node.
//
// Two things travel together because a human is already moving one of them. The
// secret is what the cluster checks the joiner against; the fingerprint is what
// the joiner checks the cluster against, and without it the joiner's first
// contact would be trust-on-first-use over a network an attacker may already
// hold (ADR-0005). Extending the string costs the operator nothing -- it was
// already opaque and already copied -- and it closes the window completely.
//
// An empty fingerprint gives the bare secret, which is what a permissive cluster
// issues: there is no handshake to pin there, and a token that looked like it
// pinned one would be claiming a guarantee it cannot make.
func FormatJoinToken(secret, fingerprint string) string {
	secret = strings.TrimSpace(secret)
	fingerprint = strings.TrimSpace(fingerprint)
	if secret == "" || fingerprint == "" {
		return secret
	}
	return secret + tokenSeparator + strings.ToLower(fingerprint)
}

// ParseJoinToken splits a token an operator supplied back into the secret the
// cluster will check and the fingerprint the joiner will pin.
//
// `pinned` is whether the token claims to name a certificate at all, and it is
// separate from whether the fingerprint is any good on purpose. A token that
// claims a pin and carries a damaged one must be refused, not quietly demoted to
// a plaintext join: a trailing dot is an ordinary way for a copied string to
// arrive truncated, and reading that as "no pin" would hand an operator who asked
// for a pinned join an unpinned one, with nothing said. So the caller branches on
// `pinned` and lets PinnedClientCredentials reject the value by name.
//
// Not pinned means the token names no certificate, which is a permissive
// cluster's token and is a plaintext join. The caller must not treat that as
// "pin nothing and use TLS anyway": no pin and no TLS is what every join did
// before this and is honest about what it is, while TLS with no pin would be
// encryption to whoever answered.
//
// The secret is returned exactly as it was given, case included -- it is compared
// byte for byte at the other end. Only the fingerprint is folded, because it is
// hex and an operator may well have moved it through something that changed its
// case.
func ParseJoinToken(token string) (secret, fingerprint string, pinned bool) {
	token = strings.TrimSpace(token)
	idx := strings.LastIndex(token, tokenSeparator)
	if idx < 0 {
		return token, "", false
	}
	return strings.TrimSpace(token[:idx]), strings.ToLower(strings.TrimSpace(token[idx+1:])), true
}
