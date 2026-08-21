# Standby is an active-active status only

`Standby` is reported in active-active clusters and never in active-passive. A node's own
record of what it holds is no longer treated as privileged knowledge, because doing so let
two members of one healthy cluster publish contradictory statuses for the same node at the
same instant (END-2289): the elected node called itself `Standby` while its peer called it
`Active`, and both were reading the same healthy cluster. The accepted cost is that `Active`
now means different things in the two modes.

## Why the old rule produced two answers

`Standby` exists to split a question `Active` used to answer twice over — *is this daemon
healthy and eligible* and *is it serving floating IPs*. Splitting it needs a count of what a
member holds, and that count is only evidence where the list is knowledge: peers self-report
their hosted addresses over `ConfigSync` in active-active and nowhere else, so in
active-passive an empty list means "this node does not know", not "this node holds nothing".

The original rule made one exception: a node's record of *itself* was authoritative in either
mode. That exception is what came apart. In active-passive it made the only row that could
ever read `Standby` the row you happened to be asking about, so the answer depended on which
node you asked. A freshly paired appliance with no floating IPs configured reached it
immediately — the elected node holds nothing, knows it holds nothing, and is the only observer
allowed to conclude anything from that.

A status an operator cannot compare between two nodes is not a status. That is the property
being bought here, and it is worth more than the distinction being given up.

## Considered options

- **Gate on whether anything is configured to serve.** Keep the local-node exception, but only
  derive `Standby` where the cluster has at least one floating IP. Fixes the reported symptom
  and leaves the contradiction alive wherever groups *do* exist and an election-promoted node's
  `ActiveIPs` is still empty — which is the `#1`/`#21` case, so it narrows the fix to the less
  dangerous half.
- **Derive the count from `config.Groups` instead of `ActiveIPs`.** Config is present on every
  node, so this is viewer-independent *and* keeps `Standby` in active-passive. Rejected because
  every node would then agree on `Standby` for a freshly paired cluster, which only helps once
  consumers render `Standby` as healthy — and the client that ships in 3.3.0 does not
  (END-2187). It remains the option to revisit if `Standby` is ever wanted in active-passive.
- **Change nothing in the daemon and fix the clients.** Leaves the daemon answering a question
  differently depending on who asks it. Every present and future consumer inherits that.

## Consequences

`Active` is mode-relative. In active-passive it means *elected*, and says nothing about whether
the node is serving anything — the pre-`Standby` meaning. In active-active it means *serving at
least one address*, with `Standby` covering healthy-and-eligible-but-serving-nothing. One wire
value, two definitions, selected by a mode an operator can change at runtime. `CONTEXT.md`
carries the definitions.

Active-passive loses the ability to say "elected but serving nothing". That state is real, not
hypothetical: on a cloud Platform, VIPs bind to Service IPs rather than floating IPs, so an
elected node may legitimately hold none at all. It is now reported as `Active`, which is the
answer its peers were already giving.

Consumers must still handle `Standby` in either mode. The mode is runtime configuration, not a
build-time fact, and active-active still emits the value — `lb_api`'s Azure active-state probe
accepts both `ACTIVE` and `STANDBY` for exactly this reason and must keep doing so.
