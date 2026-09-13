# Cluster trust is an allowlist in the config, not a certificate authority

Inter-node gRPC is authenticated and encrypted by each node holding a long-lived self-signed
certificate, and the cluster config carrying the set of certificates that are allowed to speak.
A peer is trusted because the config names it, not because something signed it. Joining a
cluster means having your certificate added to that set; being removed from a cluster means
having it dropped.

**Status: proposed. None of it is built, and what exists today is worse than nothing.**

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

Deliberately not proposed: sniffing the first bytes of a connection to serve TLS and plaintext
on one port. It works, and it is a byte-level demultiplexer in front of a cluster's control
plane, added to smooth a transition that happens once.

## Consequences

- **`GenerateCertificates` must become idempotent before anything else here is built.** A
  certificate that changes on restart cannot be in an allowlist. This is the smallest piece and
  the one everything else waits on.
- **The config grows a field that is not configuration.** A node's certificate is state the node
  publishes about itself, living in the same structure as the operator's settings. `ConfigSync`
  preserves node-local fields already, and this is the first that is node-*owned* rather than
  node-local — a distinction that does not exist today and will need one.
- **`InsecureSkipVerify` goes, and does not come back.** It is what makes the current code look
  finished. Encryption without identity would satisfy a scanner and stop no attacker.
- **Certificate rotation is a config update**, with the same propagation guarantees and the same
  failure modes as any other. It is not solved by this document, and a 10-year self-signed
  certificate defers rather than answers it.
- **Verification needs two nodes.** Every defect in `#104`-`#110` that mattered was caught
  against a real daemon, and three of them were invisible to the test suite. A handshake, a
  join carrying a pinned fingerprint, and a `permissive`→`required` flip on a live cluster
  cannot be demonstrated on one appliance, and should not be claimed without them.
