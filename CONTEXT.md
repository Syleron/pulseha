# PulseHA

The clustering daemon that keeps a set of appliances agreed on which of them is serving
traffic, and moves floating IP addresses between them when that answer changes.

This glossary covers the vocabulary of **node status** — the words PulseHA publishes about the
members of a cluster, and which an operator reads on the appliance's High availability page —
together with the named conditions those statuses combine to describe, and the words for **who
decides**: the roles a member can hold in reaching those answers, and how one answer is known
to be newer than another. It is deliberately narrow; terms are added as they are pinned down,
not in advance.

Two of the roles below are the reason this section exists. The code has used *leader* and
*coordinator* for three distinct things — the member believed to be serving, the member that
redistributes addresses, and the member that goes first in an election — across two words,
while *leader* was already listed as a word to avoid for Active. Anything read here should be
read as the canonical term; identifiers in the code are still being brought into line with it.

## Language

**Cluster Mode**:
Whether the cluster serves its floating IPs from one node or from several — `active-passive` or
`active-active`. Changeable at runtime, and the meaning of several terms below depends on it.
_Avoid_: topology, cluster type, failover mode

**Floating IP**:
An address the cluster moves between nodes, so that it answers wherever the cluster is
currently serving.
_Avoid_: VIP, virtual IP, service IP — on a cloud Platform a VIP binds to a Service IP rather
than to a Floating IP, so these are different things and not synonyms

**Floating IP Group**:
A named set of Floating IPs, assigned to a named interface on a node, that move together.
_Avoid_: pool, set, cluster IPs

**Cluster Epoch**:
A counter that orders the cluster's successive beliefs about itself, so that two members
disagreeing can tell whose view is newer rather than louder. Every claim about who is serving
carries the epoch it was formed in, and an older epoch loses.
_Avoid_: generation, term, version — "version" is already the config's, and they move
independently

**Elected Node**:
The member the cluster currently believes is Active, as of a given Cluster Epoch. A belief
rather than an observation: it is what the cluster has agreed, which is not always what is true
yet, and never for long during a transition. Paired with the epoch that formed it — an Elected
Node quoted without its epoch says nothing, because an older one is not a competing answer but
a stale one.
_Avoid_: leader, master, primary — "leader" reads as a synonym for Active, which is the one
thing it must not be confused with, and the same word is already overloaded by Coordinator
below

**Coordinator**:
The member that carries out work the cluster needs done exactly once — redistributing Floating
IPs, consolidating them onto one node when the Cluster Mode changes. Not elected and not the
Elected Node: it is derived, so that every member picks the same one without having to agree,
and it is chosen from members that are healthy whether or not they are serving. A member briefly
unreachable still counts, because handing the role over the moment a member goes quiet takes it
from the busiest member exactly when it is busiest — which is how addresses get placed twice.
_Avoid_: leader, master, orchestrator

**Election Coordinator**:
The member that goes first when the Elected Node is presumed gone, so that several members do
not promote themselves at once. A different role over a different set from Coordinator, and
deliberately drawn from members that are *not* serving — the whole circumstance is that the
serving member has stopped answering.
_Avoid_: coordinator unqualified, leader, election master — the bare word means the role above,
and the two are chosen from sets that barely overlap

**Claim**:
What a member asserts about itself: its status together with the Floating IPs it says it holds.
The two are one thing and move together, because either alone misleads — a status with no
addresses behind it tells peers a node is serving nothing, and addresses recorded against a
member that has stopped serving keep the cluster from re-placing them. A claim is an assertion
of ownership and responsibility, not a report on any interface's contents: a member can
truthfully claim an address a moment before that address is up. See
[ADR-0004](./docs/adr/0004-the-lock-covers-the-state-transition.md).
_Avoid_: assignment, holdings, hosted IPs — an assignment is what the Coordinator decided, which
is a different statement made by a different member

**Active**:
Mode-relative, and the one term here that requires knowing the Cluster Mode before it can be
read. In active-passive: the elected node — normally the single member the cluster has chosen,
whether or not it currently holds any address. In active-active: a member holding at least one
Floating IP.
A partitioned two-node cluster is the deliberate exception: each side elects itself, so two
members report Active at once. See [ADR-0002](./docs/adr/0002-two-node-availability-over-safety.md).
_Avoid_: master, primary, leader

**Passive**:
A member that is healthy and eligible for promotion but is not the elected node. Only exists
in active-passive.
_Avoid_: standby, backup, slave, secondary — "standby" in particular means something else
here, and nearly the opposite

**Standby**:
A member that is healthy and eligible for promotion and holds no Floating IPs. Reported in
active-active only, because that is the only mode in which every member's holdings are known
to every node. See [ADR-0001](./docs/adr/0001-standby-is-active-active-only.md).
_Avoid_: partial active, partially active, spare, idle

**Maintenance**:
A member the operator has deliberately excluded from promotion. Reachable and healthy, so
neither serving nor failed.
_Avoid_: out of service, drained, disabled, offline

**Unknown**:
A member that has given no healthy answer — not yet reached, or no longer answering health
checks. A statement about knowledge of the member, not about a state the member is in.
_Avoid_: offline, down, dead, unavailable, suspicious — the last two are appliance WebUI
vocabulary with no PulseHA status behind them

**Split-brain**:
Two members both Active in active-passive, each believing the other is gone. Not a status —
no node ever publishes it — but the name for a condition an operator reads off two nodes at
once. A defect in a cluster of three or more, where a majority exists and should prevent it;
the accepted behaviour of a partitioned two-node cluster, which has no majority to appeal to.
See [ADR-0002](./docs/adr/0002-two-node-availability-over-safety.md).
_Avoid_: dual active, both active — these name the symptom without saying it is unintended in
one case and not the other
