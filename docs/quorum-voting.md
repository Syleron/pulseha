# Peer voting

`BroadcastVoteRequest` asks each configured peer to evaluate a `RequestVote`
proposal. It records only the returned decision, after checking the session,
responder and epoch. Creating a gRPC client or completing a TCP handshake is not
an affirmative vote.

A session snapshots the configured member IDs when it starts and binds its epoch
on the first broadcast. Passing requires YES votes from more than half of that
fixed electorate. Unknown voters, changed ballots, expired votes and attempts to
change the proposal epoch are rejected. A changed membership view requires a new
session. The initiator evaluates and reserves its own decision through the same
local decision path as a peer.

Peers check the electorate, deadline and epoch against their own view. A
node-status proposal names the candidate; a peer refuses it when the candidate
is not Passive or an Active is still known. An IP-redistribution proposal carries
the JSON list of addresses in `subject`, rather than just their count. A peer
refuses unknown addresses or addresses still claimed by a member not beyond its
failure grace period. Generic configuration-change descriptions are not enough
to authorize a change, so that proposal type is refused by this new RPC.

Required TLS binds the request's initiator and response's voter to their
certificates, including checking that a responder is the particular member being
asked, not merely some trusted cluster member. Permissive mode checks the shared
cluster token on requests and the expected endpoint/response identity on replies;
it does not provide individual cryptographic identity or protection from a
network attacker. The wider permissive-mode administration exposure is separate
work. The old unsolicited `CastVote` network endpoint is disabled so it cannot
inject ballots into an initiator's session.

Each prepare/accept phase has a one-second peer deadline and stops waiting once
a majority replies. The complete round is also bounded by its session deadline. Timeouts, malformed replies and `Unimplemented` from older
peers contribute no votes. During a mixed-version rollout, upgraded initiators
need a majority of upgraded, agreeing members. Old initiators retain their old
behavior until upgraded; the new code cannot repair those binaries remotely.
The documented two-node exception remains unchanged.

Voting uses a durable prepare/accept protocol within each cluster epoch and vote
kind. A ballot is ordered by a monotonically increasing counter and proposer ID.
Prepare promises prohibit accepting older ballots and return the prior accepted
ballot and subject. After a majority promises, the proposer must carry forward
the highest accepted subject in that majority. This preserves any value that
could already have won, including when its acknowledgements were lost. A split
vote can recover without changing the applied cluster epoch. Expiry never erases
an acceptance or permits an older ballot to be reused.

Promises, acceptances and the proposer counter are stored in a node-specific
`votes-<node-identity-hash>.json` sidecar under the configuration directory. Writes
use a same-directory temporary file, file sync, atomic rename and directory sync;
the file is owner-readable/writable only. Corrupt/unwritable state disables voting
rather than granting without durable state. Preserve this file on restart and
restore it with the node's identity. Credential rotation does not reset it. The
last voting epoch supplies a restart floor so reboot does not reuse an older
slot. This is not durable storage for all configuration or applied cluster state.

Protocol v2 responses bind the phase and ballot as well as session, voter and
epoch. v1 responses cannot provide a prepare promise and are not counted. The
result's subject is the recovered proposal, which may differ from the request.
The election consumes that candidate; redistribution consumes only recovered
addresses that are still orphaned in its current view. A recovered proposal that
is no longer eligible fails closed; it is not silently replaced. The electorate
cannot change within an existing voting epoch.

This fixes agreement and recovery of peer votes, not the entire ownership
protocol. The automatic promotion force/fallback paths are fixed in PR #263.
Witness/fencing, stale asynchronous operations, durable configuration and atomic
application of membership changes remain separate work. No vote result proves
an unreachable incumbent released its kernel addresses. Do not delete voting
state, change membership to manufacture a majority, or advance cluster epochs as
an ad-hoc substitute for recovering a decision.

Regression tests use real loopback gRPC peers, including TLS, to cover approvals,
refusals, unavailable and hanging peers, legacy peers, mismatched replies,
identity impersonation and conflicting proposals. Quorum-manager tests cover
fixed membership, affirmative majority, expiry, duplicate votes and epoch retry
binding. These do not replace Linux network-partition/actual-address tests.

Recovery regressions exercise split acceptances, lost acknowledgements after a
majority accepts, full acceptor restart, concurrent proposers, stale accepts,
credential rotation, corrupt/unwritable state, and phase/version mismatch. A
prepare promise alone never contributes to the session's YES count.

## Automatic promotion (END-2694)

Automatic active-passive elections, including emergency fallback, require a
successful node-status vote before requesting promotion in a configured cluster
of three or more. The configured electorate does not shrink when members become
Unknown or are missing from the runtime member list. Missing voting support,
refusal, unavailable peers, and failed promotion requests leave the candidate
Passive; there is no direct status-write fallback.

Reachability recovery does not assign a role. In active-passive mode, an Unknown
peer recovers the role in its next successful, identified HealthCheck response.
A TCP connection alone cannot promote or demote it. An existing Active can thus
remain the owner when only its link to the coordinator fails; a peer reporting
Passive still needs an election before promotion. This applies with auto-failback
both enabled and disabled. ConfigSync ignores Unknown observations about the
receiving node itself, preserving its actual role for health replies; explicit
higher-epoch Passive demotions still apply.

Older HealthCheck responses omit the responder identity and role. Such a peer
can remain Unknown until a role-bearing health reply or an existing config-state
update arrives; no role is invented from reachability. Upgrade all members for
this recovery behavior. A peer's self-report is not a fencing proof or a new
ownership grant, and it does not solve stale asynchronous operations.

The election sends `ForceDemote: false`. The force flag remains an explicit
operator recovery option. The Promote admission path and asynchronous worker
also check a configured majority, independently of the quorum manager's mutable
node count. An accepted asynchronous request does not mean placement completed.

Configured one/two-node clusters retain ADR-0002's availability policy without
using the operator override. This exception never applies to two survivors of a
larger configured cluster.

This requires the explicit peer-voting protocol from PR #262; deploy that change
first (or deploy both together). Legacy peers cannot provide the required votes.

### Remaining safety boundary

This closes automatic minority promotion, not all split-brain scenarios. A TCP
failure does not establish that the incumbent stopped serving its client network.
A majority can still promote while an isolated existing Active retains addresses.
Witness/self-fencing (END-2631), durable epochs (END-2699), verified transfer
outcomes (END-2695), and stale-operation cancellation (END-2698) remain necessary
before claiming single ownership under arbitrary partitions. Live Linux tests
must partition the cluster network while retaining client connectivity and
inspect actual addresses for both Active-isolated and Passive-isolated cases.
