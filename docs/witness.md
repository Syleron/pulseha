# Running a two-node cluster with a Witness

A two-node cluster that loses the link between its nodes brings the floating IPs up on both of
them. That is deliberate: with no majority to appeal to, holding an address twice is the only
way to be sure it is held at all ([ADR-0002](adr/0002-two-node-availability-over-safety.md)).
A **Witness** is a third box that ends that state when it can prove which node should keep the
address, and leaves it alone when it cannot
([ADR-0006](adr/0006-a-witness-rules-on-overlapping-claims.md)).

## What it does, and what it does not

| What the Witness can see | What happens |
| -- | -- |
| Both nodes, each claiming the same address | It rules for the node it last saw serving; the other gives the address back, and the keeper re-announces it once the other has |
| One node only — the other answers nobody | The survivor promotes |
| Nothing, or it cannot be trusted | Both nodes stay Active, exactly as without a Witness |

The Witness never serves, never votes and is never counted: a pair with a Witness is still a
pair. A Witness that is down, unreachable or not yet trusted changes nothing — the cluster
behaves as it would without one.

**It only settles a broken cluster link if it can reach both nodes some other way.** Each node
has one `bind_address`, which is the heartbeat path. If the Witness reaches the nodes only on
that path, it cannot tell a broken heartbeat link from a dead node: it still settles a node that
has crashed or frozen, but a cluster-link-only partition goes back to both nodes Active. Give
every member a `witness_address` on the service network — where the clients are — and put the
Witness there.

## Setting it up

1. **Every member runs a build that understands a Witness.** A Witness join is refused while any
   member reports an older Witness protocol or cannot be asked, because an older build would
   count the Witness as a third node.

2. **Join the Witness box** from the box itself, with a token from the cluster:

   ```bash
   pulsectl cluster token                       # on a cluster node
   pulsectl cluster join --witness --address <node>:<port> --token <token>   # on the Witness box
   ```

   The role is fixed until the box leaves the cluster. It joins straight in as the Witness —
   it is never parked in Maintenance, holds no groups, and cannot be promoted. A cluster takes
   one Witness, and only a cluster of one or two nodes.

3. **Give every member a `witness_address`** on the service network, the Witness included, on
   the member it describes:

   ```bash
   pulsectl config set witness_address 10.0.1.11:9084
   ```

   The Witness path is TLS whatever `tls_mode` says, and a member's address is published to
   the rest of the cluster.

4. **Check that it is arbitrating.** `pulsectl status` reads the Witness as one of:

   - `Witness: arbitrating` — ready.
   - `Witness: configured but not trusted` — some member has not published its certificate yet,
     or is reached over a plaintext link. Nothing is arbitrated until every leg of the Witness
     path is verified TLS; a Report that crossed a plaintext leg could have been written by
     anyone on that network.
   - `Witness: down` — a partition leaves both nodes Active until it is back.

   and names any node with no `witness_address`.

## What it costs

Fencing is a new way for a healthy node to stop serving, so it is narrow: a node stands down only
on a Ruling against it, carried by a fresh, verified Report in which its peer answered the
Witness there and then. Witness silence never fences. A node that has stood down will not take
the address back on a brief Witness silence — it waits out a hold-down of twice the failover
limit — but it takes it back at once if the Witness reports the peer gone.
