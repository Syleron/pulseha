# PulseHA

The clustering daemon that keeps a set of appliances agreed on which of them is serving
traffic, and moves floating IP addresses between them when that answer changes.

This glossary covers the vocabulary of **node status** — the words PulseHA publishes about the
members of a cluster, and which an operator reads on the appliance's High availability page —
together with the named conditions those statuses combine to describe. It is deliberately
narrow; terms are added as they are pinned down, not in advance.

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
