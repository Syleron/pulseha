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

Each broadcast round is bounded to three seconds or the remaining session time,
whichever is shorter. Timeouts, malformed replies and `Unimplemented` from older
peers contribute no votes. During a mixed-version rollout, upgraded initiators
need a majority of upgraded, agreeing members. Old initiators retain their old
behavior until upgraded; the new code cannot repair those binaries remotely.
The documented two-node exception remains unchanged.

This fixes fabricated peer votes, not the entire ownership protocol. Epoch vote
guards are in memory, matching the current cluster epoch's lifetime. Durable
epoch recovery, fencing/leases, and the automatic promotion force/fallback paths
remain separate findings from the execution assessment. A failed vote still must
not be interpreted as proof that every existing promotion path will stop.

Regression tests use real loopback gRPC peers, including TLS, to cover approvals,
refusals, unavailable and hanging peers, legacy peers, mismatched replies,
identity impersonation and conflicting proposals. Quorum-manager tests cover
fixed membership, affirmative majority, expiry, duplicate votes and epoch retry
binding. These do not replace Linux network-partition/actual-address tests.
