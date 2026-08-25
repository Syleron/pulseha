# A partitioned two-node cluster serves from both nodes

A two-node cluster that loses communication between its members brings the floating IPs up on
both of them. This is deliberate. The invariant being protected is that **a Floating IP Group
is never dark** — held by at least one node continuously, through the partition and through the
reconvergence that follows — and in a cluster with no majority to appeal to, holding an address
twice is the only way to guarantee it is held at all. Duplicate holding is the accepted price.

**This was the intent before it was the behaviour.** Until 2026-08-25 the code did not deliver
it: a peer whose packets were being dropped was read as alive-and-still-holding, so the
isolated node refused to promote and kept refusing until the partition healed — measured at
over 150 seconds on a two-node rig, with the floating IP left on whichever node happened to
still have it and no failover available if that node were the one that had died. See
`docs/TEST-PLAN.md` defect #82. What follows describes the decision; the defect is the record
of what it took to make it true.

This is the opposite of what the rest of the daemon does, which is why it is written down.
Everything else here spends its effort preventing two nodes from holding one address: `TC-6`,
defects `#1`, `#23`, `#42`, `#68`, `enforceSingleActive`, the whole of `confirmPeerReleasedIPs`.
A reader who finds `hasQuorumLocked` returning `true` for `nodeCount < 3` without this document
will read it as an oversight and close it.

## Why a majority cannot be the answer here

Quorum answers "am I on the surviving side" by counting members it can reach. Two nodes give
that question no answer to find: each side reaches exactly one of two, neither is a majority,
and a rule that requires one produces a cluster that fails closed on every heartbeat glitch.
So `hasQuorumLocked` short-circuits — below three nodes it returns `true`, and the promotion
guard in `canPromoteWithoutConfirmedRelease` lets the isolated node claim the group.

The failure this is sized for is a **cluster-link-only** one: the members lose their heartbeat
path while both stay healthy and reachable from clients on the service network. A broken
heartbeat NIC, a firewall rule, a wedged switch port. Both nodes are fine; only their opinion
of each other is broken.

It is worth being precise about what both-Active buys in that case, because it is not what it
first appears. A gratuitous ARP is sent when an address is brought up and at no other time —
there is no periodic re-announce — so on a shared segment every client and switch holds exactly
one MAC for the address, whichever announced last. Two Actives therefore do not produce two
serving paths. They produce one arbitrary serving path, plus a duplicate address. The gain is
not doubled capacity; it is the removal of any window in which *neither* node holds the
address, which is what a safety-first rule would risk every time it guessed wrong about which
node should stand down.

## Considered options

- **Elect exactly one, by deterministic tie-break.** A rule such as "smaller node ID wins"
  needs no extra hardware and gives a single owner. Rejected because the tie-break is decided
  by a node that cannot see its peer: the winner may be the node whose service network is the
  broken one, and it will claim addresses the other was serving perfectly well. It converts a
  duplicated address into a dark one, which is the failure this decision exists to refuse.
  The implementation of this rule is present at `internal/membership/health_check.go` but is
  unreachable from the two-node election path, and must stay that way.
- **Neither node serves without confirmation.** Fail closed: a node that cannot prove it is the
  survivor releases everything. Gives a genuine single-owner guarantee and is what a cluster
  with shared storage would require. Rejected outright — a heartbeat glitch takes the whole
  service down, which is strictly worse than either alternative on the invariant above.
- **A third member as a quorum vote-counter.** Add a member that never serves, so two becomes
  three and majority arithmetic starts working. Rejected as inert for the failure being sized
  for: under a cluster-link-only partition both nodes still reach the third, so both count two
  of three and both conclude they have quorum. Counting is the wrong operation. What resolves
  the ambiguity is a member that *reports what it can see*, which is a different design and is
  tracked separately.

## Consequences

`Active` is no longer single-valued in active-passive. `CONTEXT.md` carries the amended
definition and names the condition `Split-brain`, which in a two-node cluster is expected
behaviour and in a larger one is a defect. Anything that reads member status and assumes at
most one Active — a monitoring check, a WebUI panel, an integration — is correct for clusters
of three or more and wrong for a partitioned pair.

**Reconvergence is where this gets expensive, and it is the half that needs care.** When the
link returns, `enforceSingleActive` consolidates onto the target chosen by
`ConsolidationTarget`. Both nodes hold the identical full group, so the address counts tie and
the tie breaks on the lower node ID — which bears no relation to which node the segment has
learned. The demoted node drops its addresses; the surviving node, which held them all along,
brings nothing up and so announces nothing. Half the time, by UUID ordering alone, every ARP
cache then points at a node that no longer answers. That is a dark window created by the
recovery, and it defeats the invariant more thoroughly than the partition it is recovering
from. Consolidation therefore re-places the retained addresses on the surviving node purely to
force the announcement.

**That is the reconvergence path this decision reasoned about, and it is not the one a heal
takes.** On the two-node rig the member statuses converge through `ConfigSync` before the health
checker ever observes two Actives, so `enforceSingleActive` never runs and the demotion arrives
from the state broadcast instead. The dark window is real on that path and was measured there —
the surviving Active sent zero announcements for either address it kept — so the same
re-announcement is now made when a peer moves into a status that cannot hold floating IPs while
this node stays Active. Where that re-announcement is made matters more than it looks. It cannot
hang off either end of the state broadcast: a receive-side hook fires only on the node that is
*told* of the demotion, and node-ID ordering decides whether that is the survivor, so it covers
about half of all heals; the send side cannot diff, because callers apply the state before
broadcasting it. It is detected instead on the health-check pass, which sees the settled view
every tick whoever produced it. `docs/TEST-PLAN.md` #80 carries the measurements, including the
three runs that disproved the earlier placements.

One consequence worth stating plainly, because it cuts against the framing above: the
availability this buys is **continuity, not capacity**, and it is bought by making a promotion
possible where one was previously refused. The refusal was not conservatism paying off — a pair
that cannot promote is strictly worse on this invariant than a pair that duplicates an address,
because the address a dead node holds is dark and the address two live nodes hold is not.

The decision is scoped to clusters of exactly two. At three members and above a majority exists,
quorum means something, and split-brain remains a defect to be prevented rather than a state to
be tolerated.
