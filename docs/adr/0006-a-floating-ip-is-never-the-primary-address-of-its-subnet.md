# A floating IP is never the primary address of its subnet

On every interface PulseHA manages, the interface's own address — the static the operator
configured — is the kernel's *primary* address for its subnet, and every floating IP in that
subnet is a *secondary* behind it. PulseHA enforces this itself, continuously, rather than
trusting the order in which addresses happened to arrive.

**Status: accepted, built, and verified live on an Endurance pair (`end-primary` /
`endurance-secondary`) on 2026-11-13. The fault was reproduced with `nmcli connection up` on
the Active node and repaired by the daemon in under a second. Defect #116 in
`docs/TEST-PLAN.md` has the run.**

## Why the order matters

The kernel decides which address is primary, and it decides by arrival. The first address added
in a prefix on an interface is primary; every later address in the same prefix is flagged
`secondary`. The distinction is not cosmetic:

- The primary gets the prefix route, with itself as the preferred source. A secondary gets no
  prefix route at all.
- Source selection for traffic to that subnet therefore picks the primary. Every connection a
  load balancer originates on that subnet — above all its health checks — leaves from it.

A floating IP is an address the real servers know. In Direct Routing mode they hold it on `lo`
themselves. A health check sourced from the floating IP is a packet from an address the server
owns, so the server answers it to itself, the check times out, and the server is marked down.
That is the whole product failing while every address is "up".

So the invariant is: **the static is primary, the floating IPs are secondary, on every managed
interface, at all times.** With the static first, health checks leave from the static and the
floating IPs are purely what clients connect to, which is what a floating IP is for.

## How it gets broken, and why nothing PulseHA did could prevent it

PulseHA never chooses primary or secondary. It adds an address, and the kernel files it. On a
box where the static was configured before the daemon placed anything, every floating IP is a
secondary and the invariant holds by accident of order.

NetworkManager breaks it. A profile re-apply — the appliance's `NetworkManagerApply` on boot or
on an interface save, or an operator's `nmcli connection up` — deletes and re-adds the static.
Two kernel behaviours then combine:

1. `promote_secondaries` is on (NetworkManager sets it per interface, and the appliance sets it
   for `all`). So deleting the primary does not take the secondaries down with it; it
   *promotes* the first secondary in the prefix. That is a floating IP.
2. The static comes back a moment later. It is now the second address in its prefix, so it is
   filed as a secondary, behind the floating IP that was just promoted.

The addresses are all still up. `ip addr` shows the static as `secondary noprefixroute`, the
floating IP as the plain primary, and the kernel's prefix route sourcing from the floating IP.
Health checks fail from that moment until something deletes and re-adds the floating IP.

This is what `end-primary` looked like on 2026-11-13, on `enp2s0` only, because only that
interface's profile had been re-applied.

## The decision

**PulseHA repairs the order itself, in the enforce pass, by reseating the floating IP**:
delete it and add it straight back. The delete promotes the first secondary in the prefix —
the static, once it is the first — and the add lands behind it. Where the first secondary is
another floating IP (one placed before NetworkManager put the static back), the pass looks
again and moves that one too, bounded by the number of floating addresses plus one.

It is a repair rather than a prevention because the thing that breaks the order is not ours.
NetworkManager will re-apply profiles, operators will run `nmcli`, and an appliance-side rule
not to do so on an Active node would be a convention, enforced by nobody, that the next
configurator rewrite forgets. The daemon already has a loop whose job is to make the
interfaces match what they should be; the order of the addresses is part of what they should
be.

The pieces, so a reader can find them:

- `packages/network/primary.go` — `floatingPrimaries` is the decision: a floating IP that is
  primary in a prefix which also holds a *secondary address that is not a floating IP*.
  `ReseatFloatingIPs` is the repair loop. The comment at the top of that file is the short
  form of this document.
- `internal/membership/ip_monitor_linux.go` — `keepFloatingIPsSecondary` runs it from the
  Active branch of `enforceExpectations`, after the placement loop, for every interface the
  node has expectations on. The netlink watcher triggers the pass when an IPv4 address that is
  *not* a floating IP arrives on a managed interface, which is NetworkManager re-adding the
  static, so the repair takes the pass's latency rather than up to a 30-second tick.
- `internal/membership/ip_monitor.go` — `markReseating` and `reseatSuppressed`: the watcher is
  told about the delete, because it is one the watcher would otherwise restore with a warning.
  Same shape as the release protection of defect #60.

## What is deliberately *not* repaired

- **A floating IP primary alone in its subnet.** The interface has no address of its own
  there, so there is nothing to put first and nothing to source health checks from instead.
  Reseating it would churn an address for no change.
- **A floating IP primary over other floating IPs only.** Same reason: a secondary floating IP
  is not a static.
- **IPv6.** It has no primary/secondary flag; RFC 6724 chooses the source and the problem does
  not arise in this form.
- **The decision is made against the whole configured set of floating IPs on the interface,
  not the set this node currently expects.** A surplus floating IP on its way out is still not
  the interface's own address, and reading it as one would reseat its neighbour for nothing.

## The safety gate, which is the part that must never be relaxed

**The reseat refuses to run unless `promote_secondaries` is on for the interface** — read as
the greater of `net.ipv4.conf.all` and the interface's own, which is how the kernel evaluates
it. Without promotion, deleting a primary floating IP deletes *every secondary in its prefix
with it*, and at that moment the static is one of those secondaries. The repair would take the
box off its own subnet.

When both sysctls are off, the daemon switches the interface's own on before acting, and a
sysctl that cannot be read or did not take is a refusal before any delete. This is not only
for the reseat: every ordinary failover deletes floating IPs, and on a box where one has
become primary, a failover without `promote_secondaries` would do the same damage. PulseHA
runs as root to manage addresses on the interfaces it is given, and this sysctl is part of
managing them.

## Alternatives considered

- **Add floating IPs as `/32`.** A `/32` is its own prefix, so it never competes with the
  `/24` static for primary — keepalived's folk remedy. Rejected: the floating IP would then
  have no route to its own subnet on an interface with no static in it, which is a supported
  layout; and the daemon identifies addresses by their configured prefix everywhere (the
  delete-by-held-prefix logic of defect #108 exists because of that), so it would change the
  meaning of every address in every config.
- **Add floating IPs with `preferred_lft 0`** (deprecated), so source selection skips them.
  Rejected: IPv4 source selection does not reliably honour the deprecated flag across the
  kernels the appliance ships, and it says nothing about the prefix route, which is what
  actually carries the wrong source.
- **Bind health checks to the static in HAProxy** with `source`. Rejected as the fix: it is
  per-node configuration in a config that is synchronised between nodes, it covers HAProxy and
  not the Layer 4 checkers, and it treats the symptom while leaving every other outbound
  connection sourced from the floating IP.
- **Have the appliance not re-apply profiles on an Active node.** Still worth doing — a
  re-apply also drops the floating IPs outright for the moment it takes the watcher to restore
  them — but as a courtesy, not as the enforcement. It would have to be remembered by every
  path that touches NetworkManager, in another repository, forever.

## What this binds future changes to

1. **Any new path that places a floating IP goes through, or is followed by, the enforce
   pass.** The reseat lives there. A placement that bypasses it (a direct `netlink.AddrAdd` in
   an RPC handler, say) must not assume the address it placed is secondary.
2. **Nothing adds a floating IP with `noprefixroute`, a changed prefix length, or a label**, as
   a shortcut around this. The decision keys on the kernel's own prefix grouping; an address
   that opts out of it opts out of the repair, and of the routing that makes it reachable.
3. **The promote_secondaries gate stays.** A change that removes it, or that reseats without
   first reading the effective value, reintroduces the failure mode where a repair or a
   failover takes the static down. `TestReseatRefusesWithoutPromoteSecondaries` pins this.
4. **The floating set handed to the decision is the configured set for the interface, plus
   the expectations.** `TestASurplusFloatingIPIsNotMistakenForAStatic` pins why.
5. **A reseat is never announced.** The address moves from this node to this node; the
   neighbours' ARP entries are already right, and an arping per reseat on a 200-address
   interface is defect #4's cost for nothing.

The tests that hold this in place are `packages/network/primary_test.go`, which drives the
loop against a scripted kernel applying `inet_insert_ifa`/`inet_del_ifa`'s rules, and
`internal/membership/ip_monitor_reseat_test.go`. The live check is the one recorded under #116:
re-apply the profile on an Active node, and the daemon logs `ENFORCE: floating IP had become
the primary address of its subnet; reseated it behind the interface's own address` with the
static back first and no `restoring` warning beside it.
