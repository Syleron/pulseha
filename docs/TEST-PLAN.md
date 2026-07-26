# PulseHA Cluster Test Plan

Manual verification plan for the `whitecrane` 4-node cluster. Covers the membership
invariants, floating-IP placement, and the scale behaviour surfaced on 2026-07-25/26.

Unit tests are not in scope here — run `make test` separately. This plan is for
behaviour that only appears on a real multi-node cluster with real interfaces.

## 1. Environment

| Item | Value |
|------|-------|
| Nodes | `node-1` … `node-4.whitecrane.io`, SSH as `loadbalancer` |
| Cluster IPs | 10.200.0.121-124:9083 |
| CLI | `sudo pulsectl` (daemon-local, unix socket) |
| Unit | systemd `pulseha` |
| Config | `/etc/pulseha/config.json`, key `floating_ip_groups` |

Floating IP groups:

- **`Management`** — 10.200.0.150/23, .151/23 on `enX0`. **Real addresses. Do not thrash.**
- **`Test`** — `enX1`, outside the real network range. Safe to move, drop, and break.

### 1.1 Appliance interaction (mandatory pre-step)

The appliance owns floating IP configuration. `ExecStartPost=/usr/local/sbin/lbClearRestart`
in `/etc/systemd/system/pulseha.service.d/lbClearRestart.conf` runs the `lb_api` live
configurator, whose `PulseHA::setupIPsOnGroups()` deletes any PulseHA group IP absent from
the appliance's own FIP database. IPs added with `pulsectl` are therefore wiped on the next
daemon restart.

Two ways to run this plan:

- **Recommended for PulseHA-only testing:** `sudo rm -f /run/lbBootFlag` on every node before
  starting. `lbClearRestart` exits early without the flag, so restarts leave config alone.
  Restore with `sudo touch /run/lbBootFlag` afterwards.
- **For appliance-integration testing:** leave the flag in place and add the test IPs through
  the appliance instead of `pulsectl`, so both sides agree.

### 1.2 Deploy

```bash
./build.sh          # needs -mod=mod; vendor/ is out of sync with go.mod
./deploy.sh         # installs both binaries; does NOT restart
```

Restart one node at a time (4 nodes → 3 healthy keeps quorum votes concluding).
**Restart the Active node last** and expect a failover.

### 1.3 Verify against reality, not just `pulsectl status`

```bash
for n in 1 2 3 4; do
  echo -n "node-$n: "
  ssh loadbalancer@node-$n.whitecrane.io "ip -4 -o addr show | grep -cE '10\.200\.0\.(150|151)|200\.0\.0\.'"
done
```

`pulsectl status` reports intent; only `ip addr` reports what the network sees. Every pass
criterion below is against `ip addr`.

## 2. Baseline setup

The flag is `--interface`, **not** `--iface` — cobra rejects the latter and, as with the
`set-mode` case in TC-6, a rejected command prints a usage hint and changes nothing.

```bash
sudo pulsectl group create RealTest
for id in <uuid-1> <uuid-2> <uuid-3> <uuid-4>; do
  sudo pulsectl group assign --group RealTest --node-id "$id" --interface enX0
done
for i in $(seq 152 255); do sudo pulsectl group add-ip --group RealTest --ip 10.200.0.$i/23; done
for i in $(seq 0 95);    do sudo pulsectl group add-ip --group RealTest --ip 10.200.1.$i/23; done
```

That is 200 addresses in one contiguous span of the real `10.200.0.0/23` management subnet.
In a /23 both `10.200.0.255` and `10.200.1.0` are ordinary host addresses (network is
`10.200.0.0`, broadcast is `10.200.1.255`), so the range crosses the third octet without a gap.

**Why the real subnet rather than `200.0.0.x`:** the old `Test` group used off-network
addresses, so no switch or neighbour on the segment cared about its ARP. GARP and
duplicate-address behaviour were never actually exercised. Verified free by ping sweep plus
`arping` on 2026-07-26: in use across the whole /23 are only `10.200.0.{1,10,21,22,23,24,29,50,
103,121,122,123,124}` and `10.200.1.{101,103,173}`, plus `.150/.151` reserved for `Management`.

**Keep the test IPs in their own group, not in `Management`.** Defect #10 redistributes a group
as a unit, so test IPs sharing `Management` would drag the live `.150/.151` along with them.

Budget ~5-8s per IP (see TC-5) — 200 IPs takes ~20 minutes. Back up first:
`sudo cp /etc/pulseha/config.json /tmp/config.json.bak`.

Re-verify the range is still free before reusing this plan; the scan is point-in-time and
misses powered-off kit and addresses reserved only in the appliance's FIP database.

## 3. Test cases

### TC-1 — Single-Active invariant holds continuously

**Targets:** the `enforceSingleActive` health-check invariant (commit `5b1e6bf`).

1. Confirm active-passive mode and exactly one Active in `pulsectl status`.
2. Force a second Active: on a Passive node, `sudo pulsectl node promote` (or drive a
   `BringUpIP` against it) so two nodes report Active.
3. Wait 3 health-check cycles.

**Pass:** the cluster returns to exactly one Active without operator action. The survivor is
the `ConsolidationTarget` (most-loaded Active → healthy leader → lowest-ID healthy). Demoted
nodes hold **zero** floating IPs on their interfaces.

**Fail signature:** two nodes both holding the full group — ARP-fighting over every VIP.

### TC-2 — Transient Unknown does not strip VIPs

**Targets:** the `isDemotion()` regression fix (`internal/server/server.go`).

1. Note the Active node and its full IP count.
2. Make it briefly unresponsive to peers without killing it — e.g. `sudo kill -STOP <pid>`
   for ~10s then `-CONT`, or block peer traffic with an nftables rule for one health-check
   window. Peers should mark it `Unknown`.
3. Restore and watch that node's journal.

**Pass:** the node keeps every floating IP throughout. No
`ConfigSync: LOCAL node demoted from Active` line for a `newStatus=Unknown` transition.

**Fail signature:** `ENFORCE: Removing stale floating IP from passive node … status=Unknown`
for the whole group, **including the live Management VIPs**, followed by a multi-minute
one-IP-at-a-time recovery.

### TC-3 — Config converges across all nodes

**Targets:** defect #5, the unordered `go s.broadcastFullConfigToPeers()` race.

1. From one node, add IPs back-to-back with no pause (the §2 loop does this).
2. When the loop finishes, wait 2 minutes for any reconcile.
3. Compare counts on all four nodes:

```bash
for n in 1 2 3 4; do
  echo -n "node-$n: "
  ssh loadbalancer@node-$n.whitecrane.io \
    "sudo python3 -c \"import json;print(len(json.load(open('/etc/pulseha/config.json'))['floating_ip_groups']['Test']))\""
done
```

**Pass:** all four report the same count.

**Fail signature:** counts diverge (observed 200 / 189 / 192 / 193) and **stay** diverged —
there is no periodic full-config reconcile. Diagnostic: one further single `add-ip` snaps all
nodes into agreement, which isolates it to the concurrent-broadcast race rather than to a
lost update.

### TC-4 — Failover restores the whole group

1. Record the Active node and its IP count.
2. `sudo systemctl restart pulseha` on it (or `kill -9` the daemon for an unclean failure).
3. Poll all four nodes every 30s until the total IP count returns to the group size.

**Pass:** every IP comes back on exactly one node; total count matches config; no IP is up on
two nodes at any sample.

**Record:** wall-clock from restart to full restoration, and the peak number of VIPs
simultaneously down. This is the number that matters for the SLA.

**Measured 2026-07-26 (201-IP `Test` group, clean `systemctl restart` of the Active node):**

| | |
|---|---|
| Restart issued | 01:09:59 |
| All 201 VIPs restored | 01:23:48 |
| **Restoration time** | **13m 49s** |
| Rate | 4.12 s/IP |
| Peak VIPs down | 201 (all of them) |

node-4 took over and the count climbed monotonically — 17, 34, 51, 68, 85, 102, 119, …, 187,
201 — at a dead-steady ~17 IPs per 68s sample. No IP was ever up on two nodes. So the
failover itself is *correct*; it is only the duration that fails, and the duration is entirely
TC-5's serial GARP. Restoration is fully linear in IP count with no fixed overhead worth
noting, which means the SLA for a group of N floating IPs is currently ~4N seconds of outage.

### TC-5 — IP operation cost scales linearly (known defect #4)

**Targets:** the blocking per-IP GARP.

1. Time a single `sudo pulsectl group add-ip`.
2. From TC-4, derive seconds-per-IP over the whole restoration.

**Current measured behaviour (2026-07-26, 201-IP group):**

- ~~One `AddIPToGroup` = **4.01s** wall, essentially all of it `arping`.~~ **Wrong — corrected
  2026-07-27.** That was timed one `ssh` per IP, so it measured SSH + sudo + CLI startup. Re-measured
  inside a single SSH session: **199 adds in 14s (~0.07s/IP)** with **zero** garp/arping journal
  entries for the batch. The add path goes through the IP monitor's ENFORCE branch →
  `network.BringIPup`, which does not announce at all (defect #11). Time any per-IP cost from inside
  one session, or from sampling `ip addr` over wall clock — never one SSH per operation.
- The 4s cost is real but confined to the **failover/move** paths, which call `SendGARP`
  (`packages/network/network.go:190`) inline per IP — `arping -U -c 5`, 5 packets at 1/s, blocking.
- Restoration ran at **~4s/IP**: node-3 went 12 → 193 IPs in ~11 minutes.
- Confirmed end-to-end by TC-4: a full 201-IP failover took **13m 49s** at 4.12 s/IP.
  The predicted ~13.4 min and the measured 13m49s agree to within 3%, so the cost model
  "one blocking `arping` per IP, nothing else material" is established, not inferred.

**Pass criterion once fixed:** restoration time must be substantially sub-linear in IP count —
GARP batched or issued asynchronously after the addresses are up, not once per IP inline.
Both `Server.BringUpIP` (`internal/server/server.go:~4714`) and `Member.BringUpIPs`
(`internal/membership/member.go:~204`) call `SendGARP` inside the per-IP loop.

### TC-6 — Active-active distribution converges

**Targets:** defect #2 (distribution thrash).

1. `sudo pulsectl cluster mode set --mode active-active`.
2. **Confirm the switch landed** before sampling anything:
   `sudo pulsectl status | grep Mode` must read `active-active`. A rejected command prints a
   one-line cobra usage hint and changes nothing; a sampling loop then records a perfectly
   plausible-looking active-passive cluster for its whole duration. This cost a 75-minute run
   on 2026-07-26 — the mode never changed and every sample read `n4=201`, which is correct
   active-passive behaviour and so looked like real data.
3. Sample all four nodes' IP counts every 30s for 10 minutes.

**Pass:** counts converge to within 1 of each other (`ipam.PlanMove` stops at `max-min <= 1`)
and then **stay put**. No IP appears on more than one node in any sample. No IP is absent from
all nodes.

**Fail signature:** IPs bouncing between nodes every ~20s, the same IP up on three nodes at
once, or a VIP vanishing entirely. Previously never converged over ~85s.

**Note:** the rebalance loop (`internal/membership/health_check.go:~866`) performs up to
`totalIPs` single-IP `OrchestrateIPFailover` calls **within one health-check cycle**, each
paying the TC-5 4s cost. Expect ~10 minutes to rebalance 200 IPs from 1 node to 4, with the
health-check loop blocked throughout. Distinguish "slow but converging" from "thrashing".

#### Result 2026-07-26 — **FAILED, aborted, manual recovery required**

Not thrash. **Split-brain.** Switching a 201-IP cluster to active-active put every floating
IP up on multiple nodes at once and the cluster never recovered on its own.

| Time | n1 | n2 | n3 | n4 | duplicated IPs |
|------|----|----|----|----|----------------|
| 10:54 | 35 | 0 | 0 | 201 | 35 |
| 11:01 | 84 | 89 | 63 | 116 | 109 |
| 11:02 | 101 | 127 | 78 | 134 | 156 |
| 11:05 | **201** | 85 | **201** | 178 | **201 (all)** |

440+ placements for 201 unique IPs, diverging monotonically. **The live `Management` VIP
10.200.0.150 was simultaneously up on node-1, node-3 and node-4.**

**Causal chain — this is defect #4 escalating from "slow" to "unsafe":**

1. The switch starts bulk IP movement; every IP costs a blocking 4s `arping`.
2. That starves the daemon's health checking. node-4 logged all three peers
   `unreachable` at 10:55:44 while it was mid-GARP.
3. Each node, seeing every peer dead, independently concluded the whole group was
   unowned: `ACTIVE_CHECK: redistributing 203 orphaned floating IP(s)` — logged on
   node-1 at 10:56:09 and node-4 at 10:55:45.
4. Each then claimed all 203 IPs. Nothing reconciles this, because the same GARP load
   keeps the peers looking dead.

So the serial GARP is not merely an SLA problem. Under any bulk IP operation it induces a
mutual-unreachability window long enough for every node to declare itself sole owner.

**The `Management` group is swept into redistribution along with `Test`** — it is treated as
just another group, so a 2-IP production group gets spread and duplicated across nodes.

**No working abort.** `SetMode` takes `s.Lock()` as its first statement
(`internal/server/server.go:2392`) and the in-flight IP work holds it. The revert issued at
10:58:43 was still blocked 13 minutes later; the same command on a second node returned
rc=124 (timeout). **An operator cannot abort a bad active-active switch through the CLI.**

**`ConsolidationTarget` picked a dead node.** Once the revert did land, node-4 chose
node-1 — whose daemon was stopped at the time — as the consolidation target and began
releasing its IPs to it (201 → 150) before the target was started.

**Recovery required manual intervention** and could not be done through `pulsectl`:
`systemctl stop pulseha` on 3 nodes (two needed SIGKILL escalation — they were wedged in
GARP and could not process SIGTERM), `ip addr del` of every stray address, hand-editing
`"mode"` in `config.json` on the stopped nodes, then a staggered restart.

**Do not re-run TC-6 on a cluster carrying live addresses** until the GARP is made
non-blocking and the orphan-reclaim path requires quorum. On this cluster it duplicated a
production VIP for roughly 15 minutes.

### TC-7 — Capacity caps placement

**Targets:** `pulsectl node capacity`, `ipam.Distribute` / `ipam.HasCapacity`.

1. In active-active, `sudo pulsectl node capacity 10 --node-id <node-2>`.
2. Wait for rebalance to settle.
3. `sudo pulsectl node capacity 0 --node-id <node-2>` to remove the cap; wait again.

**Pass:**
- Capped node holds **≤ 10** IPs.
- The remaining IPs are spread across the uncapped nodes, still balanced among themselves.
- No IP is left unplaced while any node has spare capacity.
- Setting capacity back to 0 lets the node take a full share again.

**Also check:** set capacities so total capacity < total IPs. `ipam.Distribute` returns the
overflow as `unplaced` — confirm those IPs are reported rather than silently dropped.

### TC-8 — Return to active-passive consolidates cleanly

1. `sudo pulsectl cluster mode set --mode active-passive`.
2. Wait 3 health-check cycles past the grace period.

**Pass:** exactly one Active; that node holds **all** group IPs; the other three hold none.
This is TC-1's invariant exercised through the path that originally broke it — `SetMode`
demoting at switch time while a late `BringUpIP` re-promotes a node.

## 4. Teardown

```bash
sudo pulsectl cluster mode set --mode active-passive
for i in $(seq 20 219); do sudo pulsectl group remove-ip --group Test --ip 200.0.0.$i/23; done
sudo touch /run/lbBootFlag          # if removed in §1.1
```

Confirm `Management` still holds 10.200.0.150/23 and .151/23 on exactly one node, and that no
`200.0.0.x` addresses remain on any `enX1` (orphans survive group deletion — check `ip addr`,
not just `pulsectl`).

## 5. Coverage

| Case | Defect | Status |
|------|--------|--------|
| TC-1, TC-8 | #1 two-Active in active-passive | Fixed `5b1e6bf`, needs live re-verification |
| TC-2 | #6 Active self-strips VIPs on Unknown | Fixed, needs live verification |
| TC-3 | #5 config diverges under concurrent mutation | **Open** |
| TC-4, TC-5 | #4 serial 4s GARP per IP | **Open** — quantified 2026-07-26: 13m49s to fail over 201 IPs |
| TC-6 | #2 active-active — **split-brain, not thrash** | **Open, blocker.** Every node claimed all 203 IPs; live VIP triple-homed; manual recovery |
| TC-6 | #7 GARP starvation → mutual unreachability → orphan reclaim | **Open, root cause of #2** |
| TC-6 | #8 `SetMode` unabortable — blocks on `s.Lock()` | **Open** — no CLI escape from a bad switch |
| TC-6 | #9 `ConsolidationTarget` selects a node with a dead daemon | **Open** |
| TC-6 | #10 `Management` group redistributed like any other | **Open** — spreads live VIPs |
| TC-7 | capacity enforcement | **Not run** — blocked by TC-6 |
| TC-8 | return to active-passive | **Not run** — blocked by TC-6 |
