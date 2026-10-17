# A Witness ends a two-node partition by ruling on overlapping Claims

**Status: accepted, not yet built (END-2631).**

ADR-0002 lets a partitioned two-node cluster serve from both nodes, because with no majority to
appeal to, holding an address twice is the only way to be sure it is held at all. A **Witness**
is a third box that changes that answer only when it can prove something: it reports what it
can see of both nodes, and for every address two nodes claim at once it issues a **Ruling** on
which one keeps it. The loser is **Fenced** — it gives the address back. When there is no
Witness, or it cannot be reached, or it cannot be trusted, ADR-0002's behaviour is unchanged.
That fallback is the floor this decision is built on, not something it replaces.

```
 node, peer not visible to it
        │
        ├─ Witness unreachable / untrusted / silent ─────────► both Active (ADR-0002)
        │
        ├─ Witness Report: peer not visible ─────────────────► promote
        │
        └─ Witness Report: peer answered a live round-trip
               │
               ├─ no address claimed twice ──────────────────► carry on as you are
               │
               └─ addresses claimed twice → Ruling
                      ├─ ruled for me ──► keep; re-announce once the peer is seen to release
                      └─ ruled for peer ► Fenced: release those addresses
```

## The rule is about Claims, not liveness

The obvious rule — "the Witness says my peer is alive, so I stand down" — fences **both** nodes
in the very failure this exists for. In a cluster-link-only partition each node asks about the
other, each hears "alive", each stands down, and the group is dark: worse than the duplicate
ADR-0002 accepts. Liveness is a precondition for a Ruling, never the reason for one. The
conflict is two Claims on one address, so that is what is ruled on:

- **active-passive** — two Active Claims overlap on every address; the Fenced node becomes
  Passive and releases everything.
- **active-active** — two Actives is the healthy state; only the groups held by both overlap,
  and the Fenced node releases those and stays Active with what it held before.

## The tie-break is history the Witness saw, not the Cluster Epoch

The Witness keeps whichever Claim it last saw as legitimate: the last Elected Node in
active-passive, the last placement from the Coordinator in active-active. That is the node the
network has already learned, so keeping it moves no traffic. Node ID decides only when the
Witness has no earlier observation, as after its own restart mid-partition.

## The Witness is a member that is never counted

It is configured, trusted under ADR-0005 and receives config, but it is excluded from cluster
size, quorum, the voting electorate, placement, and both coordinator roles. Counted, it would
turn a two-node cluster into a three-node one and switch off `hasQuorumLocked`'s `nodeCount < 3`
path — a node that lost both its peer and the Witness would count one of three and refuse to
promote, the fail-closed outcome ADR-0002 rejects. The role is fixed at join, one per cluster,
and refused on clusters of three or more, which are END-2325's territory.

Two consequences of that are easy to undo by accident:

- **The status is not the guard; the role is.** A Witness that is up holds a status of its own,
  so anything that accepts only Active or Passive leaves it out by default. But a Witness that is
  down reads Unknown, and candidate selection, the Election Coordinator and the Coordinator all
  consider Unknown members. So the role travels on the member, a Witness can only ever be given
  Witness or Unknown whoever writes its status, and every election, coordinator, placement and
  promotion path reads a member list with the Witness taken out. A decision path that ranges over
  every member is the bug.
- **A Witness may only join a cluster whose every member understands one.** An older binary
  counts every configured node, so to it a pair with a Witness is three, and its two-node path is
  off. The join asks each member for the Witness protocol version it speaks and refuses on any
  member that is older or cannot be asked.

## Evidence has to be live, and it has to be trusted

- **Every Witness Report is formed by fresh round-trips**, each with a deadline strictly inside
  the asker's own, so a slow peer reads as *not visible* rather than as an unreachable Witness.
  A wedged daemon still accepts TCP connections, and a status remembered from the last health
  check still says a SIGSTOPped Active is serving — either would keep the survivor from
  promoting while the address answers nobody. The proof of life is the peer reporting its own
  Claim; the Witness's memory is used for the tie-break and nothing else.
- **The Witness talks to nodes over the service network**, on an address each node declares for
  it. Over `bind_address` alone it shares the heartbeat path, cannot tell a broken cluster link
  from a dead node, and resolves nothing in a cluster-link-only partition.
- **That listener requires TLS whatever `tls_mode` says.** A Ruling can make a healthy node
  release its addresses, and the service network is reachable by much more than the cluster; a
  plaintext Ruling is a one-packet outage. ADR-0005's permissive phase exists to migrate links
  that already existed, and this one did not. A report that did not arrive over a verified
  channel from the configured Witness counts as no report.

## Fencing must not open the dark window it was meant to close

- **The winner re-announces** each address it kept once a later Witness Report shows the
  loser has released it — not on the Ruling, which would race the loser's last announcement —
  with an age-out in case the Witness disappears in between. ADR-0002 measured why: clients
  follow whoever announced last, and the winner, having held the address throughout, would
  otherwise announce nothing. The health-check re-announcement cannot cover this, because the
  winner cannot see its peer.
- **A Fenced node promotes again** at once on a report that its peer is not visible, but on
  Witness silence only after a hold-down longer than peer-failure detection; otherwise a flaky
  Witness swings the service between the nodes once a cycle. Seeing the peer again, or a
  restart, clears the condition; it is never persisted.

## Considered options

- **Peer alive → stand down.** Rejected above: it fences both nodes in a cluster-link-only
  partition.
- **Higher Cluster Epoch wins.** Rejected: each side of a partition bumps its epoch alone, so
  the numbers count each side's own history. The node that promoted *into* the partition has
  the higher one and would win over the node that served throughout. The epoch still travels
  in a Witness Report, so a stale report can be refused.
- **The Witness pushes demotions.** Rejected for pull: a lost command leaves an unknown state,
  where a missing answer falls into ADR-0002's fallback; and a node's Claim keeps one writer,
  itself, under its own lock (ADR-0004).
- **A Witness as a runtime toggle, like Maintenance.** Rejected: it changes how the cluster is
  counted, and a toggle lets the arbitrator become a candidate mid-partition.

## Consequences

The Witness's value depends on where it can reach the nodes from, which is a deployment
decision. A Witness reached only over the heartbeat path still arbitrates a crashed or frozen
node, but a cluster-link-only partition goes back to ADR-0002 — and that has to be said
wherever the feature is documented. A round-trip proves the daemon answers, not that the
address passes traffic; a daemon alive over a dead data path is out of scope here.
