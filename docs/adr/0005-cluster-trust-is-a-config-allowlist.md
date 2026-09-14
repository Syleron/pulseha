# Cluster trust is an allowlist in the config, not a certificate authority

Inter-node gRPC is authenticated and encrypted by each node holding a long-lived self-signed
certificate, and the cluster config carrying the set of certificates that are allowed to speak.
A peer is trusted because the config names it, not because something signed it. Joining a
cluster means having your certificate added to that set; being removed from a cluster means
having it dropped.

**Status: accepted, and built. Steps 1, 2 and 3a landed first (#255, #256, #257) and are
verified live. Steps 3b and 4 — the listener credentials, the client's trust-set check, the
token's pinned fingerprint and the cluster-scoped flip to `required` — are built and **not yet
verified live**, which for this design is the only verification that counts. A cluster left on
the default `permissive` is still plaintext, which is the migration working as intended, not a
stall. The ordering below was corrected twice, both times from building it; see the bootstrap
section.** What existed
before any of this was worse than nothing, and that description applied to the listener until
step 4; the paragraphs below describing it as serving no credentials are kept as the record of
what was found.

## What is actually there now

`internal/client.Connect` takes a `tlsEnabled` argument, and **all 17 call sites pass `false`**.
That looks like a flag someone forgot to flip. It is not: `false` is the only value that works,
and four separate things would have to change before `true` could.

- **No listener serves TLS.** All three are `grpc.NewServer()` with no credentials. A client
  offering TLS to them fails at the handshake.
- **Every node generates its own CA, on every start.** `GenerateCertificates` is unguarded, so
  `ca.crt` is rewritten each time the daemon comes up — observed on a lab appliance, CA written
  at `07:55:31` against a process start of `07:55:30`. Node A's certificate is signed by an
  authority node B has never seen, and B's copy of its own changes whenever B restarts.
- **Nothing distributes a CA.** The join flow does not carry one, and no other path does either.
- **`InsecureSkipVerify: true`** in the client's `tls.Config`, so even a connection that
  somehow completed would be encryption without identity.

So the cluster token, every `ConfigSync`, every health check and every floating-IP command cross
the wire in clear, and the scaffolding that appears to address it never could have. The risk of
leaving it in place is not only the plaintext: it is that the next reader flips the flag,
watches a cluster stop talking, and concludes TLS "doesn't work here".

## Why a certificate authority is the obvious answer and the wrong one

The reflex is a cluster CA: create it at `CreateCluster`, sign a certificate for each joining
node, distribute the CA certificate over the join. It is what the abandoned code was reaching
for. It fails on custody.

**Somebody has to hold the CA key.** Put it on every node and compromising any single node mints
an identity for every other — the blast radius of one appliance becomes the cluster. Put it on
one node and that node is required for every join, which in a two-node cluster means losing the
wrong node loses the ability to replace the other. Neither is acceptable for a product whose
premise is that any node may fail.

An allowlist has no key to hold. Each node's private key never leaves the node that generated
it, there is nothing whose theft grants the power to impersonate, and the thing that must be
distributed — a public certificate — is not a secret at all.

**Revocation falls out for free, and it is the part a CA does badly.** Removing a node from a
cluster is already an operation this codebase performs. With an allowlist, removing it from the
config *is* the revocation, propagated by the machinery that already carries every other config
change and that `#43` taught to propagate removals reliably. With a CA, a removed node's
certificate stays valid until it expires, and the alternative is a CRL nobody will operate.

The cost is that trust is O(n) rather than O(1): the config carries one certificate per node,
about 1.2 KB each. On a cluster whose size is bounded by `ADR-0002`'s availability argument that
is not a number worth optimising.

## The bootstrap problem, which is the only hard part

A node that has never spoken to the cluster has no way to tell the cluster from an attacker
imitating it. Handing over the trust set on first contact is trust-on-first-use, and TOFU over a
network an attacker may already hold is not authentication — it is a coin flip that happens to
usually land right.

**The token already solves this, because a human already carries it.** `pulsectl cluster token`
produces a secret that an operator copies to the joining node out of band. Extending that token
to carry the **SHA-256 fingerprint of the joinee's certificate** costs the operator nothing —
the string is longer, and it is already opaque — and it closes the window completely. The joiner
pins that fingerprint, verifies the presented certificate against it with a
`VerifyPeerCertificate` callback, and only then sends the token. There is no first-use window
because the first use is already authenticated by something a human moved.

This also inverts the current exposure in a way worth stating: today the token is the thing
protected by nothing. Afterwards, the token is what protects everything else.

**Corrected 2026-09-14, from building it.** The paragraph above puts the fingerprint in the token
as its own step, ahead of the listener credentials. That order does not work, and the reason is
one sentence long: **there is no handshake to pin until the listener serves TLS.** Over a
plaintext join a fingerprint carried in the token is an integrity check — it catches a stale or
wrong-cluster config — and it is *not* authentication, because an attacker able to sit in the
path does not need to forge a certificate, only to relay one. Landing it early would put a
security-shaped mechanism in place that does not yet provide the security its name implies,
which is the failure this whole document exists to stop repeating.

So the pinning moves to sit with the credentials, and the two arrive together or not at all. What
*is* separable, and landed first as step 3a, is the joining node handing over its **own**
certificate with the request: that needs no handshake, it only needs somewhere to record the
answer, and it removes the gap where a node is in the cluster's config before it is in the
cluster's trust set.

**What building it added, which the paragraphs above missed entirely.** Pinning the joinee's
certificate is only half the bootstrap, and it is the easier half. The other half is that **the
cluster has to let an unknown certificate in far enough to ask** — a joiner is by definition not
in the trust set of the cluster it is joining, so a listener that refused an unnamed certificate
at the handshake made such a cluster one nobody could ever join. That is not something the token
can fix from the outside.

The answer is that the two ends stop being symmetric, and the refusal moves one layer up. The
dialling end verifies in full: a node never sends a request to a server the config does not name.
The listening end demands a certificate and verifies nothing at the handshake, and an
**authorisation interceptor** then checks that certificate against the trust set on every call,
letting an unnamed one reach `Join` and nothing else — still gated by the token. Encryption comes
from the handshake; authorisation is a separate question, asked where the answer depends on what
is being asked for. The handshake cannot make that distinction, because it does not yet know.

This is a weakening of "the listener refuses any peer the config does not name" only in wording.
An unnamed peer can complete a handshake and then do exactly one thing, and that one thing needs
a secret a human carried. The alternative was a cluster that could be created and never grown.

## Migration, which is where a live cluster gets broken

A TLS-only node cannot talk to a plaintext peer, and an HA cluster is upgraded one node at a
time. The transition therefore cannot be a single flag.

Two phases, both carried by the config mechanism that already exists:

1. **`tls_mode: permissive`.** Nodes generate a stable certificate (once — the guard that is
   missing today), publish it into their own config entry, and it propagates. Everything still
   speaks plaintext. The cluster accumulates the trust set while nothing depends on it. Safe to
   land, safe to sit in for as long as an estate needs.
2. **`tls_mode: required`**, a cluster-scoped key applied through the same path `SetMode` uses.
   Listeners rebind with credentials; clients verify against the trust set.

**Phase 2 refuses unless every member is healthy and every member has a certificate in the
config.** The precondition is the whole safety argument: the flip is delivered over the
plaintext channel it is about to remove, so a node that misses it becomes unreachable and gets
failed over. Requiring unanimity before starting is how that is avoided, and `#103` is the
record of what config divergence costs when a node is left behind.

**The ordering inside phase 2, learned from building it.** The change is written and stamped,
then pushed to every peer over a connection opened *explicitly in clear*, and only then applied
to this node. Handing it to the peers on the cluster's new terms cannot work and is the trap
worth naming: the config has already been written, so the ordinary dial would offer TLS to peers
that are still plaintext, every one of them would refuse, and the message would never reach the
nodes it is about to cut off. A test with a plaintext peer catches it; nothing else does.

There is still a window between the push and the last peer applying it, during which a flipped
node cannot reach an unflipped one. It is bounded by broadcast latency — sub-second on the
healthy cluster the precondition insists on — against a failover that needs `fo_limit` (10s by
default) of continuous failure, so the cluster rides through it. That margin is why the
precondition demands health and not merely a count of certificates.

A peer that does not take the push is **reported, not rolled back**. This node and the peers that
did take it are consistent; the one that did not is isolated and needs an operator, which is this
document's accepted consequence. A revert would have to reach the peers that have already
flipped, over a wire they have stopped accepting in clear, and would replace one divergence with
a worse one.

Deliberately not proposed: sniffing the first bytes of a connection to serve TLS and plaintext
on one port. It works, and it is a byte-level demultiplexer in front of a cluster's control
plane, added to smooth a transition that happens once.

## Consequences

- **`GenerateCertificates` must become idempotent before anything else here is built.** A
  certificate that changes on restart cannot be in an allowlist. This is the smallest piece and
  the one everything else waits on. *(Done, #255 — and the ordering held: step 2 was tested once
  without it and the certificates churned on every restart.)*
- **The order this lands in, corrected once by contact with the code.** 1: the certificate stops
  moving. 2: the trust set accumulates under plaintext, depending on nothing. 3a: the joiner's
  certificate travels with the join. Then 3b and 4 **together** — listener credentials, client
  verification against the trust set, the token's fingerprint pinned at the handshake, and the
  cluster-scoped flip to `required`. The move of 3b is explained above; the shape of the rest is
  unchanged. *Amended again, from building them:* 4 landed one commit ahead of 3b, and for that
  commit a node could not join a cluster that was already `required` — which is what the missing
  half costs, stated plainly rather than discovered later. Both are in now.
- **The config grows a field that is not configuration.** A node's certificate is state the node
  publishes about itself, living in the same structure as the operator's settings. `ConfigSync`
  preserves node-local fields already, and this is the first that is node-*owned* rather than
  node-local — a distinction that does not exist today and will need one.
- **`InsecureSkipVerify` goes, and does not come back** — meaning encryption without identity
  goes. It is what makes the current code look finished, and it would satisfy a scanner and stop
  no attacker. *Amended from building it (step 4):* the flag itself is set, paired in the same
  `tls.Config` with a `VerifyPeerCertificate` that requires the peer's certificate to be one the
  config names, byte for byte. What the flag switches off is the question "did an authority vouch
  for this name", asked of a cluster that has no authority and dials by address; what replaces it
  is stricter than what it disables. The flag alone remains the defect. The pair is the design,
  and the code says so at the line so that a future reader grepping for the flag finds the reason
  rather than the bug.
- **Certificate rotation is a config update**, with the same propagation guarantees and the same
  failure modes as any other. It is not solved by this document, and a 10-year self-signed
  certificate defers rather than answers it.
- **Verification needs two nodes.** Every defect in `#104`-`#110` that mattered was caught
  against a real daemon, and three of them were invisible to the test suite. A handshake, a
  join carrying a pinned fingerprint, and a `permissive`→`required` flip on a live cluster
  cannot be demonstrated on one appliance, and should not be claimed without them.
