# Voting recovery and execution review — 27 September 2026

Scope: PR #262's peer voting and failed-round recovery, followed through the
active-passive promotion and active-active orphan redistribution callers in
stacked PR #263. This is not completion of the other assessment tickets.

## Execution chain

| Boundary | Checked behavior | Evidence / remaining limit |
| --- | --- | --- |
| Configuration → session | Session records configured member IDs. Lost peers do not shrink the denominator. | Quorum-manager regression tests; caller rejects changed electorate before consuming a decision. Dynamic membership agreement remains separate. |
| Session → prepare | A durable, ordered ballot is proposed; each peer promises before replying. | Real gRPC, TLS identity, wrong-phase/version, unavailable and hanging-peer tests. Prepare replies never count as YES votes. |
| Prepare → accept | A majority's highest accepted value is carried forward. The returned subject may differ from the request. | Split-vote and concurrent-proposer tests; majority accepted with lost replies recovers the same candidate after all acceptors restart. |
| Accept → result | Only responses for the exact session, phase, ballot, voter and epoch count. Acceptance is synced to disk first. | Fixed-majority, expiry, stale-ballot, corrupt/unwritable-state and duplicate/conflicting-ballot tests. |
| Restart → convergence | Promises/counter survive reboot and token rotation. Voting epoch floor is restored before listeners and address writers start; convergence reads respect that floor. | Restart/promise and convergence regression tests. Applied config and address ownership are not made durable by this sidecar. |
| Result → automatic promotion | Consume the recovered candidate; require current epoch/electorate, Passive eligibility and no intervening Active. Never set the operator force flag. | Membership caller tests in #263, including changed epoch/config, maintenance, emergent Active, failed RPC and emergency fallback. |
| Result → redistribution | Consume the recovered address set, and require those addresses still belong to the current orphan set. | Recovered-subset and non-orphan refusal tests. The protocol agrees an address proposal, not a fenced destination placement transaction. |
| Promote → worker | Admission and worker check configured majority. Accepted RPC is not completion. | Minority admission tests. Worker cancellation, concurrent promotions and stale effects after dispatch remain END-2698. |
| Worker → old owner release | Attempts demotion/confirmation before takeover. | Reviewed call chain. Unreachable is not proof of released kernel addresses; END-2631 fencing remains necessary. |
| Transfer → kernel → bookkeeping | Existing helpers still have response/partial-failure gaps. | END-2695 remains open. No claim of verified source-release/destination-acquire atomicity. |

## Validation

- Race-enabled internal, packages, command and unit suites passed on the macOS host.
- Recovery/contending-proposer scenarios passed 20 repeated race-enabled runs.
- Linux amd64 daemon/CLI build and integration-test compilation passed.
- Development CI now matches slash-containing branch names and pull requests;
  it runs the existing build, ordinary tests, race tests and Linux integration suite.
- Docker Desktop briefly accepted a disposable container probe, then its engine
  became unavailable. No local Linux packet-partition or actual-address ownership
  test is claimed. CI results must be checked for the pushed revisions.

## Acceptance boundaries

A higher voting round is not an ownership lease. An accepted proposal is never
forgotten just because its reply/deadline was lost. A recovered value that is no
longer eligible fails closed rather than being substituted; progressing beyond
an already-chosen but unexecutable operation needs an explicit state transition,
not an arbitrary epoch bump or deletion of the voting file.

The sidecar covers durable voting promises, accepted values and counters. Epochs
and electorate changes still originate in the existing convergence/configuration
machinery; this is not a replacement for a replicated configuration/ownership
log. Operator force, permissive-mode administrative access (END-2696), old
binaries, and outstanding asynchronous address writers remain outside this
protocol's safety guarantee.

Before claiming partition-safe ownership, run a three-node Linux test with only
the cluster link cut (keep client connectivity), separately isolating the Active
and a Passive. Assert actual interface addresses and traffic, then rejoin. Keep
the deliberate two-node duplicate-ownership expectation as a separate test.

## Follow-up: asymmetric reachability (2 October 2026)

The review of #263 found that unconditional Unknown-to-Passive recovery with
`auto_failback` enabled could broadcast a demotion of a reachable incumbent and
release its addresses before a replacement was authorised. Recovery now uses the
peer's identified HealthCheck role, while ConfigSync preserves the receiver's
own role against Unknown gossip. A role changed during a probe is left alone.
Explicit demotions and quorum requirements for new promotions remain in place.

Regression coverage exercises real gRPC role replies, absent/wrong responder
identity, TCP-only reachability, role changes during the probe, and repeated
higher-epoch Unknown gossip followed by the incumbent's health reply. The full
race suite and vet pass. The Docker test builder now follows Go 1.26; the protobuf
test request is cloned rather than copying its embedded lock.

A four-node Linux Docker run with `auto_failback: true` kept the coordinator and
incumbent on separate nodes. After a stable 20-second baseline, bidirectional
iptables drops blocked only their cluster-network link for 65 seconds, followed
by 20 seconds of healing. The service network stayed connected. Actual interface
sampling found one unchanged holder in all 19 baseline, 61 partition and 19 heal
samples, with zero dark or dual-holder samples. A separate service-only probe
received all 206 ICMP packets. Logs confirmed coordinator-to-incumbent failures
and another peer recovering the incumbent's reported Active role. An earlier run
changed owners during setup and was discarded because its cut missed the actual
incumbent. This one-address bridge test validates the reported regression; it is
not a full fencing, reboot, or large-address-set acceptance test.
