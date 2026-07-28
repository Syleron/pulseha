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
3. Sample all four nodes' IP counts every 30s for 10 minutes. Prefer the **node-local
   sampler** introduced in run 16 (see the run-16 harness notes) over sampling via SSH per
   tick — SSH sampling skews the timeline, and a shared temp file fabricates duplicates.

**Pass:** counts converge to within 1 of each other (`ipam.PlanMove` stops at `max-min <= 1`)
and then **stay put**. No IP appears on more than one node in any sample. No IP is absent from
all nodes.

**Fail signature:** IPs bouncing between nodes every ~20s, the same IP up on three nodes at
once, or a VIP vanishing entirely. Previously never converged over ~85s.

**Expected timing as of 2026-07-28:** convergence to `50/50/50/51` in **~90s**
(runs 8-14). `ipam.PlanMoves` batches per node pair, so a 150-address redistribution is 3
`OrchestrateIPFailover` calls taking ~23s, not one call per address. The earlier note here —
up to `totalIPs` single-IP calls in one health-check cycle at ~4s each, ~10 minutes to
rebalance 200 addresses — described the code before `348ca0f` (batch GARP) and `ddcd433`, and
no longer applies. Anything materially slower than ~90s is now itself a finding.

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

> **Superseded 2026-07-28.** Both preconditions are met — GARP is batched (`348ca0f`) and a
> live peer unconditionally blocks promotion (`6791885`). TC-6 has since converged cleanly
> (runs 8-14). Keep live VIPs out of the group under test anyway; defect #10 means the
> `Management` group is still redistributed like any other.

#### Result 2026-07-27 (2nd run) — FAILED again, with an exact root cause

Re-run on the freshly reset cluster against the 201-IP `RealTest` group on the real
`10.200.0.0/23` range. `Management` was empty this run, so no production VIP was at risk.

Baseline at 00:23:00 was clean: node-1 Active with all 201, nodes 2-4 at zero, 201 unique /
201 placements / **0 duplicates**, all four configs agreeing at 201.

`cluster mode set --mode active-active` issued on node-1 at 00:23:17. It **produced no
output and was killed by a 120s client-side timeout** — it never returned successfully.

Observed progression — a ~5 minute split-brain window, then a collapse to a single node:

| Time | n1 | n2 | n3 | n4 | placements | unique | duplicated |
|------|----|----|----|----|-----------|--------|-----------|
| 00:23:00 | 201 | 0 | 0 | 0 | 201 | 201 | 0 |
| 00:26:17 | 201 | 70 | 0 | 0 | 271 | 201 | 70 |
| 00:27:48 | 201 | 96 | 1 | 0 | 298 | 201 | **97** |
| ~00:28 | 201 | 99 | 1 | 0 | 304 | 201 | **103** |
| 00:28:55 | **0** | 108 | 1 | 0 | 109 | **109** | 0 |
| 00:30:14 | 1 | 123 | 1 | 0 | 125 | **125** | 0 |

Two distinct failures, in sequence:

**Phase 1 (00:23:30 – 00:28:55) — split-brain.** Up to **103 of 201 addresses were
simultaneously up on two nodes** for about five minutes.

**Phase 2 (from 00:28:55) — mass outage.** node-1 dropped all 201 addresses in one step, and
because node-2 can only re-add them at ~4s each, **unique coverage fell to 109 of 201** — i.e.
**92 addresses were down on every node at once**, recovering only slowly. The group was never
correctly distributed; it converged toward node-2 holding everything, which is
active-passive behaviour, not the 50/50/50/51 split `ipam` planned.

**Root cause — corrected.** This is *not* the "GARP starvation → unguarded orphan reclaim"
path recorded under defect #7. The journal gives an unambiguous chain:

```
00:23:24.394  node-1  Received request to change cluster mode to: active-active
00:23:24.394  node-1  Redistributing IPs for active-active mode        [holds s.Lock()]
00:23:24.397  node-2  RPC BringUpIP on iface enX0 for 51 IP(s)         ← its assigned share
00:23:29      node-2  HEALTH_CHECK: cluster state changed - node-1(unreachable)
00:23:31      node-2  Status change: MC-LB-node-1 became unreachable (was Active)
00:23:31      node-2  ACTIVE_CHECK: No active node found in cluster, initiating election
00:23:32      node-2  ELECTION: Voting election succeeded, promoting candidate=MC-LB-node-2
00:23:32      node-2  PROMOTE_ASYNC: No active node found in cluster
00:23:32      node-2  PROMOTE_ASYNC: Demotion decision shouldDemote=false
00:23:32      node-2  PROMOTE_ASYNC: Successfully promoted local node to Active
00:23:32      node-2  RPC BringUpIP on iface enX0 for 201 IP(s)        ← claims the WHOLE group
```

node-1's own journal fills in the other half. Its redistribution planned a stride-4
interleave — 50 IPs for itself, 51 for node-2, 50 each for nodes 3 and 4 — and **every peer
assignment RPC timed out**, because each peer was itself blocked doing serial GARP:

```
00:23:54  node-1  Failed to assign IPs to node hostname=MC-LB-node-2 error="DeadlineExceeded"
00:24:24  node-1  Failed to assign IPs to node hostname=MC-LB-node-4 error="DeadlineExceeded"
00:24:54  node-1  Failed to assign IPs to node hostname=MC-LB-node-3 error="DeadlineExceeded"
00:24:54  node-1  Bringing up IPs on interface count=50 iface=enX0
```

**GARP runs even when there is nothing to do.** node-1's own 50 were already up, and the
log shows the 4-second cost being paid regardless:

```
00:24:54  NETWORK: IP existence check ip=10.200.0.155 exists=true existingIface=enX0
00:24:54  NETWORK: IP already exists on target interface (nothing to do) ip=10.200.0.155
00:24:58  Successfully brought up IP on interface ip=10.200.0.155/23      ← 4s later
```

That is 200s spent re-announcing 50 addresses that never moved — the clearest single
demonstration of defect #4, and it is what keeps node-1 unresponsive long enough for the
election to fire.

The sequence is **election-driven self-promotion during a lock-induced false death**:

1. `SetMode` takes `s.Lock()` and runs redistribution synchronously, so node-1's daemon
   stops answering health checks (defects #4 + #8 combined).
2. Peers conclude there is **no Active node at all** — not that a peer is merely slow.
3. node-2 wins an election and promotes itself. Because it sees "No active node found",
   **`shouldDemote=false`** — so it never issues `MakePassive` to node-1.
4. node-1 is not dead, only wedged, and still holds all 201 addresses. Every address
   node-2 brings up is therefore a duplicate.
5. When node-1 finally drains its GARP backlog it learns from `ConfigSync` that node-2 is
   Active, so it is now non-Active — and the non-Active branch of
   `IPMonitor.enforceExpectations` (`internal/membership/ip_monitor_linux.go:231-283`)
   removes **all** cluster floating IPs, not just the ones it was not assigned. All 201 go
   down in one step, which is why coverage collapsed to 109 unique at 00:28:55.
6. node-2 then re-adds the shortfall at ~4s per address, so the group stays partially down
   for many minutes. This is the same asymmetry as defect #4: teardown is instant, recovery
   is serial.

The critical defect is step 3: **an incumbent that looks *absent* is never demoted, only an
incumbent that looks *present* is.** A promotion that cannot confirm the old Active has
released its IPs must not bring those IPs up. `enforceSingleActive` (`5b1e6bf`) does not
help here because the wedged node cannot participate in its own demotion.

**Failed IP assignments are never retried.** node-1 logged one `DeadlineExceeded` per peer
and moved on. Nodes 3 and 4 consequently held **zero and one** address respectively for the
entire run — the 100 addresses `ipam` planned for them were simply dropped on the floor.
Even if the split-brain were fixed, active-active would still not distribute, because the
distribution step has no retry and no reconciliation to notice it failed.

**The mode change was lost entirely.** After the dust settled, `pulsectl status` on node-2
reported `Mode: active-passive` with node-2 as the sole Active holding the whole group. So
the switch to active-active did not merely fail to distribute — **it never took effect
anywhere except node-1's on-disk config**, while still causing a five-minute split-brain and
a 92-address outage on the way through. The most likely reason is that node-1 was killed by
the client timeout before it ever broadcast the new mode, and nothing retries it.

**The diverged mode is unrepairable through the CLI, and it makes the node oscillate.**
node-1 was left Passive but with `"mode": "active-active"` on disk. In that state the refresh
path still hands it an active-active assignment (29 stride-4 addresses), and a single ENFORCE
pass then does both halves of the contradiction:

```
00:39:02  Adding IP to interface ip=10.200.1.3/23  → Successfully brought up IP
00:39:02  ENFORCE: Node is not Active, removing floating IPs status=Passive
00:39:02  ENFORCE: Removing stale floating IP from passive node ip=10.200.1.3/23
00:39:02  ENFORCE: Successfully removed floating IP ip=10.200.1.3/23
```

It adds an address and deletes it again in the same pass, forever, walking the list
(`0.239 → 0.211 → 1.3 → 1.43`). Each iteration momentarily duplicates an address the real
Active legitimately holds — which is why the duplicate count never reached a clean zero and
the offending address kept moving. On real on-subnet addresses this is continuous ARP churn.

Attempts to repair it:
- `mode set --mode active-passive` **on node-1** → `rc=124`, timed out after 180s (defect #8).
- `mode set --mode active-passive` **on node-2** → `cluster is already in active-passive mode`,
  `rc=0`, **no change**. `SetMode` early-returns on the local node's view of the mode, so it
  can never repair cross-node divergence.
- Recovery therefore required `systemctl stop pulseha` on node-1, hand-editing `config.json`,
  clearing strays with `ip addr del`, and restarting — the same manual procedure as 2026-07-26.
  The `systemctl stop` again exceeded 120s (the wedged daemon does not process SIGTERM promptly).

**A Passive node can stop enforcing entirely, holding a duplicate indefinitely.** node-3
picked up `10.200.1.94` early in the run and still held it 20+ minutes later, dual-homed with
node-2. All the preconditions for cleanup were satisfied — the address was on `enX0`, it *was*
present in node-3's `RealTest` group (201 IPs), and node-3's status *was* Passive — yet
node-3's journal showed **no ENFORCE lines at all** over a 4-minute window, only
`Health check failed for MC-LB-node-1 ... connection refused` repeated once per second. Its IP
monitor had effectively stopped reconciling. Manual `ip addr del` was required. Worth checking
whether a peer that is hard-down starves the monitor loop, since that is the only unusual
condition node-3 was under.

Also confirmed this run:
- **The mode never propagates before the switch completes.** Mid-switch, node-1's
  `config.json` read `"mode": "active-active"` while nodes 2, 3 and 4 all still read
  `"active-passive"` — the cluster ran in two modes at once. `SetMode` persists the mode
  before consolidating, so on-disk mode is not a reliable state indicator (defect #8).
- **`BringUpIP` treats an already-assigned address as success** — node-2 logged
  `BringUpIP: IP assignment failed but IP is now present on <iface>` repeatedly. The
  duplicate is detected at the syscall level and then swallowed, so nothing escalates.
- Only node-2 self-promoted this time; nodes 3 and 4 accepted the election result and
  stayed at zero. The 3-way claim seen on 2026-07-26 is the worse case, not the only one.

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

Row statuses are maintained as of **2026-07-28**. Where a row and a dated narrative
section below disagree, the narrative is the record of what was actually observed;
the row is the summary. "Unconfirmed" means the code the defect described has since
been rewritten by another fix, but the defect itself was never re-tested — treat it
as neither fixed nor reproduced.

| Case | Defect | Status |
|------|--------|--------|
| TC-1, TC-8 | #1 two-Active in active-passive | **Fixed `5b1e6bf`, verified live.** `enforceSingleActive` runs every health-check cycle, coordinator-gated |
| TC-2 | #6 Active self-strips VIPs on Unknown | **Fixed, verified live.** `isDemotion(old, new)` — only Passive and Maintenance count; Unknown is left to the health checker |
| TC-3 | #5 config diverges under mutation | **Open, and broader than recorded.** Reproduced 2026-07-27 under *concurrent* mutation: 200 rapid `add-ip` calls left the four configs at 200/190/188/193, still diverged after 90s. **Run 17 reproduced it under *serial* mutation too** — 200 one-at-a-time `add-ip` calls left node-3 at 196, precisely the last four added (`10.200.1.92-95`), not self-healing after 30s+. So "serial adds converge" is false; a serial batch can lose its tail on one node. One further serialized `add-ip` snaps all four into line. Fire-and-forget `go s.broadcastFullConfigToPeers()` per mutation, no version guard. **Test-setup consequence:** always verify the configured count on all four nodes before trusting a baseline |
| TC-4, TC-5 | #4 serial 4s GARP per IP | **Fixed `348ca0f`, verified live.** `SendGARPBatch` announces a set with bounded fan-out. Mode switch `RC=124`/~13 min → **`RC=0` in 9s**; full 201-address bring-up ~13 min → **under 30s** |
| TC-6 | #2 active-active distribution — split-brain, then non-convergence | **Fixed, verified live (runs 8-14).** TC-6 converges to `50/50/50/51`, `placements=205 unique=205 duplicated=0`, stable across three samples, ~90s. Took five independent fixes (`348ca0f`, `bf1c3eb`, `ddcd433`, `0bdf7b9`, `655d5b7`, `65dedb9`) — each hid the next |
| TC-6 | #7 GARP starvation → false death of the Active | **Fixed via #4/#8, verified live.** The Active no longer blocks long enough to be marked dead, so the election that drove the split-brain no longer fires. Note the original attribution to orphan reclaim was wrong — the trigger was a promotion election |
| TC-6 | #12 promotion never demotes an *absent* incumbent (`shouldDemote=false`) | **Fixed `8ffc1c1` + `6791885`, verified live.** Release is verified against observable interface state and a live peer unconditionally blocks promotion. TC-6 run held `duplicated=0` through the wedge window with 69 logged aborts |
| TC-6 | #21 `MakePassive` returns `Success: true` without releasing anything | **Fixed `8ffc1c1`, verified live.** A boolean from `MakePassive` is no longer treated as evidence; interface state is checked |
| TC-6 | #26 in active-active, nodes claim the whole group instead of their share | **Fixed `ddcd433`, verified live.** The expectation set was rebuilt from the whole configured group at every seeding site, mode-blind; the enforce loop now recomputes per tick. Node-4: 769 enforce bring-ups per 4 min → 2 |
| TC-6 | #13 failed `assign IPs` RPCs are never retried | **Open** — nodes 3 and 4 got 0 and 1 IP; 100 planned placements dropped silently. Same shape as #21/#31 |
| TC-6 | #14 non-Active branch strips *all* group IPs, not just unassigned | **Unconfirmed.** The branch was rewritten by `ddcd433` and `releaseUnassignedIPs`; teardown-vs-recovery asymmetry is also much smaller now GARP is batched (#4). Never re-tested in isolation |
| TC-6 | #15 GARP re-announces addresses that never moved | **Unconfirmed.** `348ca0f` removed the amplification (the 4s-per-IP inline cost), but whether a no-op address is still announced was not re-checked |
| TC-6 | #16 `BringUpIP` swallows "IP already present" as success | **Open** — free split-brain detector currently discarded. **Prerequisite for #29**: a deliberate make-before-break duplicate would be indistinguishable from a split-brain in the logs until this is fixed |
| TC-6 | #8 `SetMode` unabortable — blocks on `s.Lock()` | **Fixed `348ca0f`, verified live.** `MakeActive` no longer holds the member lock across bring-up; `SetMode` defers the new Active's bring-up until `s.Lock()` is dropped. `RC=0` in 9s, 26s from a clean baseline |
| TC-6 | #9 `ConsolidationTarget` selects a node with a dead daemon | **Open** — reachability is not part of the selection. Not re-tested |
| TC-6 | #10 `Management` group redistributed like any other | **Open** — spreads live VIPs. Live groups probably need pinning to active-passive semantics regardless of cluster mode |
| TC-6 | #17/#19 interrupted `SetMode` loses the mode change and leaves the cluster mode-diverged, with no CLI path back | **Open** — `mode set` early-returns on the local node's view, so it cannot repair divergence. Less reachable now `SetMode` returns in 9s rather than timing out, but nothing retries the propagation |
| TC-6 | #22 promotion storm — `performPromotionAsync` re-fires ~1/s for the same target | **Open** — no idempotence or in-flight dedup |
| TC-6 | #23 freshly started node self-promotes on an unconverged memberlist | **Open, blocking for test setup.** Every staggered cold start of all four nodes produced two nodes holding the full group (196/201/160 duplicates across three attempts). Workaround: rolling restart, never cold-start all four |
| TC-6 | #24 `ForceDemote` is not operator intent | **Partly fixed `6791885`** — a live peer is unconditionally fatal. **Still overloaded:** a minority-side election can bypass the quorum check for a provably-down peer. Needs a field distinct from `ForceDemote` (proto change) |
| TC-7 | capacity enforcement | **Unblocked, never run.** Active-active is stable as of runs 8-14, so the precondition now holds |
| TC-8 | return to active-passive | **Passes (run 16, 2026-07-28)** via `mode set` as specified — `duplicated=0` across all 294 sampled seconds, consolidation in 4s |
| TC-8 | #27 reverse switch runs two whole-group consolidations onto two different targets | **Fixed, verified live (run 15)** — `SetMode` propagates mode + states in one ConfigSync before moving any address; both deciders now pick the same target. See "Root cause (defect #27)" below |
| TC-8 | #28 a demoted peer's own IP monitor re-claims the whole group | **Fixed `2ae8189`, verified live run 16 (2026-07-28).** TC-8 ran `duplicated=0` across all 294 sampled seconds and consolidated in 4s; node-3/node-4 logged the `ConfigSync: LOCAL node demoted from Active` line that the fix resurrected. Original diagnosis: The peer could hold `mode=active-passive` and `self=Active` because `ConfigSync` adopted the incoming epoch *before* testing whether the payload was decisive, so `decisive` was structurally always false and every peer's view of the local node's own status was discarded — including a real demotion. See "Root cause (defect #28)" below |
| TC-6, TC-8 | #30 the post-load VIP reconcile brings up the whole group, mode-blind | **Fixed `c50f027`, verified live run 17 (2026-07-28).** Steady-state duplication is **0 across 617 consecutive scored seconds** (was 5-14), and no node ever re-claims the group — node-1 only descends after the switch, the other three never exceed 50, across three config reloads. See "Result 2026-07-28 (run 17)" below. Original diagnosis: The claim set now goes through the same assigned-subset filter as every other seeding site (`filterToAssigned`, extracted so the two cannot diverge again); the release set stays whole-group on purpose. Original diagnosis: `loadInitialMembers` spawns a goroutine that 500ms later brings up every group IP if the local node reads Active, with no active-active filtering. It runs on every full ConfigSync, so in active-active each Active peer re-claims the whole group and the enforce loop's `releaseUnassignedIPs` has to undo it — a likely source of the residual 5–14 steady-state duplication |
| TC-8 | #32 `config.Reload()` unmarshals over the live `*Config` while readers are in flight | **Fixed `291564f`, not yet verified live.** `Reload()` now returns a fresh deep clone with disk state loaded over it and leaves the receiver alone; `Server.Reconfigure` and main.go's SIGUSR2 handler swap their pointer, and `Reconfigure` also calls `memberList.UpdateConfig` so the member list and health checker don't keep the stale pointer. Signature changed to `Reload() (*Config, error)`. Detector is `packages/config/reload_race_test.go` — **20/20 fail before, 20/20 pass after**. The earlier "22/40 before, 3/40 after" figure from the demotion tests **did not reproduce** and should not be used: those are 0/40 at HEAD and `make testrace` is 0/20 either side of the fix. **Residual, deliberately open:** ~278 unsynchronized `s.config` pointer reads in `internal/server` — the pointer race predates this (ConfigSync and RESYNC already swap it) but `Reconfigure` runs on every full ConfigSync, so the swap is now more frequent. Readers always see a fully-built immutable config, so closing it properly means an accessor or `atomic.Pointer` across all 278 sites |
| TC-6 | #33 batched GARP announces addresses the node does not hold | **Open, found run 17.** 173 `failed to GARP. exit status 2` in one convergence (n2=74, n3=49, n4=50). Confirmed by hand: `arping -U` exits 0 on a held address, 2 on an unheld one, and 40 in parallel all succeed — so it is a stale announce set, not a fan-out limit. Carries #11's risk: an address that did come up can be missing from a successful announcement |
| TC-6 | #34 the enforce loop retries releases of addresses it does not hold | **Open, found run 17.** 925 `ENFORCE: failed to release unassigned floating IP ... cannot assign requested address` on node-1 in one switch, ~18 per address. Harmless in effect, but it is the noise that would hide a release that mattered. Same shape as #33. Related: node-4 got `RPC BringDownIP for 201 IP(s)` for a group it held none of |
| TC-6 | rebalance dual-homes a batch for ~20s | **Open, found run 17.** Destination brings up before source releases: n1&n4 shared 46 addresses for 20s, n1&n3 shared 2 for 22s. The opposite order from #29, so the two bound the problem from both sides. Also non-monotonic: node-2 released a batch it had already claimed (50 → 0 → 25 → 50), putting 50 addresses on no node for ~8s. Only visible at 1s sampling; earlier 30s sampling could not see it |
| TC-8 | #29 per-address down-then-up gap during consolidation | **Open** — run 15 measured `unique=149/205` for ~2s at t+1: 56 addresses momentarily on no node. Run 16 reproduced it at 57 for ~2s. Not per-address: `SetMode`'s goroutine runs all demotions to completion before the activation, by design. Fix shape is handover (make-before-break) vs failover |
| TC-8 | #31 a ConfigSync cycles the gRPC listener; the refused `BringDownIPs` is never retried | **Open, found run 16.** node-1's release RPCs to node-3 and node-4 both got `connection refused` mid-switch although neither daemon restarted — `Reconfiguring PulseHA server` tears the listener down. Only a `Warn`; nothing retries. Benign only because the demoted peer self-releases via its own ConfigSync, which is the path `2ae8189` restored — so this is very likely #28's proximate trigger |

---

## Result 2026-07-27 (3rd run) — TC-6 split-brain closed

Fixes deployed and md5-verified on all four nodes (`c3446325782c`):
`8ffc1c1` + `6791885` (plus `a740474`, `2dd7a65` which were the two non-working
attempts described below).

### Outcome: the promotion-over-a-wedged-Active split-brain is PREVENTED

Switch triggered 10:03:25 on node-2 (the Active), wedged as always
(`PULSECTL_RC=124`, no output). Throughout the whole wedge window the sampler
recorded **`duplicated=0`**. node-4 logged 69 aborts:

```
PROMOTE_ASYNC: Aborting promotion - cannot confirm unreachable node released
  its floating IPs unconfirmed_node=049-b22-093-2d3 target=125-6de-27a-3f4
  peer_still_alive=true have_quorum=true reachable_nodes=3
  error="rpc error: code = DeadlineExceeded desc = context deadline exceeded"
```

Compare the 2nd run, where the same test dual-homed 103–201 addresses.

### It took three attempts; the first two were silently ineffective

1. `a740474` branched on the error from `Server.MakePassive`. That method never
   returns a non-nil error for a remote failure — every path returns
   `(&Response{Success: false, Message: ...}, nil)`. So `err == nil` was always
   true and the wedged-peer detection was dead code. Proof: with node-2's daemon
   *stopped*, node-4 logged `Confirmed unreachable node released its floating IPs`
   for a peer it had never reached. Fixed in `2dd7a65` by issuing the RPC directly
   so the gRPC status survives.
2. `2dd7a65` then got an honest transport answer but a dishonest application one.
   `MakePassive` built its drop set from `s.config.Nodes[id].IPGroups`, which the
   in-flight redistribution had already emptied, so `len(ipsToDrop) > 0` was false,
   nothing was released, and `Success: true` was returned anyway. Fixed in `8ffc1c1`
   (defect #21).
3. Even then it was bypassed, via `force_demote` (defect #24). Fixed in `6791885`.

### New defects

| ID | Defect | Status |
|----|--------|--------|
| #21 | `MakePassive` reports success without releasing anything — drop set from the node's *assigned* groups, and `BringDownIPs` errors only `Warn`ed | **Fixed** `8ffc1c1` — drop set is now every group; release verified against the interfaces |
| #24 | `force_demote` is not operator intent: `HealthChecker.tryForcePromote` (`health_check.go:1956`) sets it on *every* election-driven promotion, disabling the guard on exactly the TC-6 path | **Fixed** `6791885` — a live peer is unconditionally fatal. **Partly open:** still overloaded, so a minority-side election can bypass the quorum check for a provably-down peer. Needs a field distinct from `ForceDemote` (proto change) |
| #25 | Refusing promotion leaves the group *unserved*, not served by the incumbent — node-2 released nearly everything before wedging (6 of 201 up anywhere at 10:06:47) | **Open** — this is the cost of the #21/#24 fix. Batching GARP (#4/#8) is what removes it |
| #26 | In active-active, nodes claim the whole group instead of their share — correct 50/51/50/50 split appeared at 10:09:40 (n2=48, others 0), then node-4 took all 201 (10:11:41), then node-1 too (10:13:39) | **Open** — post-convergence redistribution, distinct from promotion |
| #22 | Promotion storm — `performPromotionAsync` re-fires ~1/second for the same target, each repeating the full IP failover orchestration | **Open** — no in-flight dedup |
| #23 | A freshly started node self-promotes on an unconverged memberlist. node-2 3s after boot: `prev_active="" reachable_nodes=4 unconfirmed_incumbents=0` while node-4 held all 201 | **Open, blocks testing** — reproduced on *every* staggered cold start (196/201/160 duplicates in three attempts). The #21/#24 guard cannot catch it: peers are recorded as a definite `Passive`, not `Unknown` |

### Test-harness notes

- **Do not cold-start all four nodes** to build a baseline (#23). Start one, let it
  take the group, then stop/strip/start the others.
- A sampler sharing one temp file across concurrent runs fabricates duplicates —
  use `mktemp` per invocation.
- The config key is `floating_ip_groups`, not `groups`.
- Deploying without rebuilding after a commit is silent; check the binary md5
  against the local build before trusting any run.

#### Result 2026-07-27 (4th run) — the wedge is fixed; distribution still fails

Two fixes landed and were verified live on whitecrane (`Management` empty throughout, so no
production VIP was ever at risk; only the verified-free `RealTest` range was in play).

**Defect #4/#8 — serial GARP wedges the Active — FIXED.** `SendGARPBatch` announces a whole
set with a bounded fan-out instead of one address at a time; `MakeActive` no longer holds the
member lock across the bring-up; `SetMode` defers the new Active's bring-up until the server
lock is dropped, and runs it after the demotion releases rather than before.

| Measurement | Before | After |
|---|---|---|
| `pulsectl cluster mode set` | `RC=124`, wedged ~13 min | `RC=0` in 9s, and 26s from a clean baseline |
| Full 201-address bring-up | ~13 min | under 30s |
| Duplicated during the switch | 103–201 | 5–14 |

This also removes the outage window defect #25 described: promotion no longer has to choose
between a split group and an unserved one, because the Active stops falsely appearing dead.

**Defect #2 — active-active thrash — root cause found and fixed, but TC-6 still fails.**
The node-2 logs showed a three-second loop: `BringUpIP: Transitioned local node to Active` →
`ConfigSync: LOCAL node demoted from Active` → `ENFORCE: Removing stale floating IP` → repeat.
Cause: every node broadcast its whole view every three health checks at `epoch+1`, on
*unchanged* state. The epoch orders authoritative decisions, so a keepalive claiming a new one
meant the most recent speaker always won — a peer with a stale view could undo a coordinator
assignment simply by speaking last. Fixed by broadcasting the nudge at the current epoch, and
by having ConfigSync ignore a peer's equal-epoch opinion of the local node's own status (the
rule already applied to the maintenance flag).

Verified: all four loop signatures above are now **zero** in the logs, and steady-state
duplication fell from 188–196 to 5–14.

TC-6 nevertheless still **fails**. Over 17 minutes from a clean baseline the cluster converged
slowly but never met the pass criteria:

```
12:18  22/49/86/189   duplicated=125
12:23   7/77/127/113  duplicated=116
12:29  33/95/80/75    duplicated=76
12:33  24/71/31/59    duplicated=5    unique=180
12:35  37/53/45/68    duplicated=14   unique=189
```

Two distinct failures remain:
  - **Never balanced to within 1.** 37/53/45/68 against a target of ~50 each.
  - **Addresses absent from every node.** `unique` fell to 180–189 of 205, so 12–21 configured
    addresses were down cluster-wide — an explicit fail signature for this case.

Not yet root-caused. Two hypotheses were tested and **disproved**, so don't spend time on them
again: the bring-down is not a silent no-op (141 moves, 141 `Bringing down IPs on old node`,
141 `Orchestration completed successfully`, zero failures, zero grouping errors), and
`BringDownIP` does already maintain the local node's own `ActiveIPs` in active-active
(server.go, the mirror of `BringUpIP`). The remaining suspects are the per-move
`OrchestrateIPFailover` reporting success without verifying the source released — the same
shape as defect #21, and the fix there is the model — and the fact that per-node IP
assignments are never propagated to the nodes themselves, since `BroadcastClusterState`
carries statuses and leases but no assignment map.

**Defect #23 (startup race) reproduced twice more, and it contaminated a run.** Restarting all
four nodes after a deploy produced 188–196 duplicates that had nothing to do with the code
under test. Starting node-2 as the second node also had it claim all 202. The single-Active
invariant did consolidate it within ~30s, cleanly. The rule stands: never cold-start all four;
start one, let it take the group, then bring the rest up, and verify a clean baseline before
reading anything into a run.

Harness notes for next time:
  - `zsh` does not word-split an unquoted `$SSHOPT` — use an array, or a script file.
  - Check the binary md5 on every node against the local build before trusting a run. Two
    deploys this session were verified this way (`d6b0c10c4be1`, then `c526b903b480`).

### Result 2026-07-27 (runs 5-7) — expectation bug fixed; the real blocker is assignment propagation

The previous run's two suspects were both wrong. The engine of the TC-6 distribution
failure was the IP monitor's *expectation set*, not the failover path.

**Root cause (fixed, commit `ddcd433`).** Every site that seeded the monitor's expected IPs
rebuilt them from the whole configured group, ignoring cluster mode — `initializeExpectedIPs`
at daemon start, and four copies in the server, one of them inside `OrchestrateIPFailover`
itself. In active-passive that is correct: the sole Active owns the group. In active-active
the group is shared, so every Active node's enforce tick re-added all 201 RealTest addresses.
The copy inside `OrchestrateIPFailover` was the worst of them: it ran on every move and
cleared the accurate per-IP set that `BringUpIP` had just recorded, widening it straight back
out to the whole group.

Measured on node-4 over four minutes before the fix: **1485 enforce passes, 769 enforce
bring-ups**, no promotion and no election involved. After: **2 bring-ups on node-4, 0 on
node-3.** That churn is gone.

Two changes were needed, because fixing the five seeding sites was not enough on its own:
  - Derive the expectation set from the node's own assignments in active-active, via one
    helper per package instead of five duplicated whole-group loops.
  - Recompute in the enforce loop each tick. The set has several writers and the case that
    matters has *none* of them fire: a node that was the sole active-passive Active keeps the
    whole group across a switch to active-active, and nothing recomputes it. Observed
    directly — node-2 reported 199 expected addresses after the switch.

Also fixed: "assigned nothing" and "no restriction" were collapsed by a `len()==0` check, so
an active-active node awaiting its first assignment claimed the entire group.

**TC-6 still fails, and the reason is now identified.** Per-node IP *assignments* are never
propagated to the assigned node. `BroadcastClusterState` carries statuses and leases, no
assignment map, and `rebalanceActiveActive` updates only the **coordinator's** copies of
`src.ActiveIPs`/`dst.ActiveIPs`. So each node's own `ActiveIPs` disagrees with the
coordinator's decision. Measured mid-run: node-3's own `ActiveIPs` held **1** address while it
physically served ~100 that the coordinator had assigned it; node-2's sat at ~201 decaying one
address at a time.

That decay rate is the second half of the problem: **switching to active-active leaves the
whole group assigned to the former sole Active**, and the coordinator drains it one
`OrchestrateIPFailover` at a time at roughly one address per 11s. Draining ~150 addresses that
way takes ~27 minutes, which is why every run so far has been observed mid-convergence rather
than converged. `SetMode` needs to do an initial redistribution instead of relying on
incremental rebalancing.

**A release pass was tried and reverted — do not retry it in this form.** Making the Active
branch of `enforceExpectations` bring down group addresses it holds but is not assigned looks
like the obvious complement to the passive-branch cleanup, and it does fire (154/56/171
releases across nodes). But it acts on `member.ActiveIPs`, which per the above is only
authoritative **on the coordinator**. On node-3 it therefore tore down ~100 addresses the
coordinator had legitimately assigned, and `unique` fell to 184/205 — real addresses down
cluster-wide. Any such teardown must wait until assignments are actually propagated; on a
cluster carrying live VIPs it would drop traffic. The guard that skips the pass when nothing
is assigned was necessary but nowhere near sufficient.

Order of work implied: propagate assignments (needs a proto field) → give `SetMode` an initial
redistribution → only then consider an Active-side release pass.

Harness notes:
  - A rolling restart (passives first, Active last, one at a time) preserved a clean 205/205
    baseline — no defect #23. Prefer it to the full stop/strip/start reset when only the
    binary changed.
  - Building with `-mod=mod` to work around the vendor drift rewrites `go.mod`/`go.sum` (it
    bumped the go directive to 1.25.0). Revert those two files before committing.
  - Verified md5 on every node against the local build for all three deploys this session
    (`2f841c12315a`, `6ac61c718741`, `e53fb888e239`).

## Result 2026-07-27 (runs 8-14) — TC-6 passes

The switch to active-active now converges. Run 13: `50/50/50/51` RealTest addresses across
the four nodes, `placements=205 unique=205 duplicated=0`, all four Active, cluster online,
stable across three samples. Convergence takes about 90 seconds; the three rebalance batches
themselves take 23.

Five faults, each independently sufficient to prevent convergence. The order below is the
order they were found, which is also the order they had to be fixed — each one hid the next.

1. **`SetMode` re-placed the group instead of recording where it was.** The active-active
   branch cleared every member's `ActiveIPs` and called `RedistributeIPs` on the whole group.
   The former sole Active still physically held all 201 addresses, so three peers were told to
   bring the same addresses up: ~150 duplicates before any node had made a decision. It also
   ran those bring-ups under `s.Lock()`, so the call took 17-21s and the daemon was unavailable
   throughout — the `pendingIPWork` deferral that the active-passive branch already used was
   never applied here. Fixed by `seedActiveActiveAssignments`, which records the current Active
   as owner of the group it can host and clears the rest; the coordinator's rebalance moves
   them out, and that path brings each address down on the source. The switch call is now
   instant.

2. **The seed had to reach the coordinator, not just the node handling the request.** Run 10
   seeded correctly on node-1 (`ip_count=201`) and node-2 — the coordinator — still logged
   `redistributing 201 orphaned floating IP(s)`. Every node derives the same answer from the
   config it has, so `ConfigSync` seeds on the mode transition too. No wire-format change was
   needed: `ConfigSyncRequest.config` is free-form JSON, which also corrects the earlier note
   that assignment propagation needs a proto field.

3. **One IP-failover round trip per address.** `rebalanceActiveActive` applied
   `ipam.PlanMove` one address at a time, about one every eleven seconds — ~27 minutes for the
   ~150 moves the switch needs, which is why every earlier run was observed mid-convergence.
   `ipam.PlanMoves` now plans the whole pass and aggregates per node pair, and
   `OrchestrateIPFailover` already accepted a batch. Three calls, 23 seconds. A test asserts
   the batched plan leaves the cluster in the same state as the incremental loop.

4. **Concurrent coordinators.** Coordinator is a local decision — lowest-ID node *this node*
   considers healthy — so it is only single-writer while every node agrees on who is healthy.
   Bulk IP work breaks that: a node moving fifty addresses is slow enough that peers mark it
   Unknown, and each peer that does appoints itself. Run 8 had all four nodes redistributing
   at once, ~170 addresses claimed by more than one owner. Two changes: `clusterCoordinator`
   and the stranded-IP sweep now wait out `FailOverLimit` before acting on a node's silence —
   the same patience active-passive failover already applies — and `reconcileActiveActive`
   requires `viewStableCycles` unchanged health checks before it acts. The 60-check counter
   reset was removed so `checksWithoutChange` really counts consecutive unchanged cycles.

5. **`resolveDuplicateAssignments` brought down addresses that were not duplicated.** A node
   listing the same IP twice produced `IP 10.200.0.180/23 assigned to both MC-LB-node-3 and
   MC-LB-node-3, removing from MC-LB-node-3` — same node both sides — and the address was
   brought down, so a node lost an address it was the only owner of. Collapse the list instead;
   only a *different* node's claim is a conflict. `rebalanceActiveActive` also no longer
   credits the destination with addresses it already records.

6. **The Active branch only ever added.** With the above fixed, node-2 sat at 172 live
   addresses against a correctly-computed expectation of 50, every surplus one also up on its
   new owner, and 3069 futile enforce bring-ups. `releaseUnassignedIPs` now brings down group
   addresses the node is not assigned. This is the pass that was reverted in runs 5-7 as
   unsafe, and it was: it keys off `ActiveIPs`, which back then diverged from reality. Faults
   1-5 are what made the record trustworthy. It is also what made faults 1-5 measurable — with
   nothing releasing, every over-assignment looked like a duplicate rather than a decision.

Transient loss during convergence, reduced but not eliminated. Run 13 dipped to
`unique=172/205` before recovering. Two writers discarded a node's assignments on a
*transient* non-Active status, which in active-active is a missed health check rather than a
demotion — `BroadcastClusterState` nil'd `ActiveIPs`, and the monitor's non-Active branch
stripped every cluster floating IP (defect #14). Both are now active-active aware: the map
survives the blip, and a non-Active node releases only what it is not assigned. A genuinely
failed node still gives everything up, via the coordinator after `FailOverLimit`.

Run 14 with those in: worst dip `unique=199/205`, peak duplication 13 (was 89-141), settling
to `50/50/50/51`, `placements=205 unique=205 duplicated=0` and holding across three samples.
The remaining dip is inherent to the move ordering rather than a defect: `OrchestrateIPFailover`
brings an address down on the source before bringing it up on the destination, so each moved
address is briefly absent. The alternative ordering would duplicate it instead, which is worse.
A rebalance move is not a failover and could in principle hand over without a gap, but that
needs the destination to be ready before the source releases — a larger change than this, and
worth its own defect entry rather than being folded in here.

Harness notes:
  - Sampling every 10s is optimistic; `state.sh` opens 12 SSH connections per pass and takes
    1-2 minutes when the nodes are busy. Read the sampler log rather than assuming a cadence.
  - `nohup ... &` from a backgrounded Bash tool call dies with the call. Run the sampler as
    the background command itself.
  - `cut -c1-125` on journal lines truncated `ip_count=201` to `ip_count=2` and briefly looked
    like a real defect. Widen the cut before believing a suspicious number.
  - md5 verified on all four nodes against the local build for all six deploys this session
    (`456d5e3d41fc`, `413acef2bd4a`, `ef7cf869c985`, `b110efc2c489`, `6204d0a70c2e`,
    `57548b8698f8`, `c3e6b43e80ba`).

### Reverse transition (active-active → active-passive), run 14

Returns to a correct baseline but with a large transient. Immediately after
`cluster mode set --mode active-passive` the whole group was up on **two** nodes
(node-2 and node-3 both at 202 live, `duplicated=201`); `enforceSingleActive`
resolved it and the cluster settled to node-2 Active with 202, the other three
Passive with 1 each, `placements=205 unique=205 duplicated=0`, exactly one Active,
cluster online. Total time about 100 seconds.

So the invariant holds and the end state is right, but `SetMode`'s active-passive
branch consolidates onto `ConsolidationTarget` and every address is doubly-claimed
for a minute and a half, which on a cluster carrying live VIPs is an ARP fight over
all of them.

#### Root cause (defect #27) — two consolidations, not a stray promotion

**The hypothesis recorded above was wrong.** Nothing "promotes a second node", and
this is not `enforceSingleActive` losing a race with the switch. The journals for
the 19:52:00 switch on 2026-07-27 show **two whole-group consolidations running at
once, onto two different targets**:

```
19:52:00  node-1  Received request to change cluster mode to: active-passive
19:52:00  node-1  Consolidating floating IPs onto ... hostname=MC-LB-node-4   ← target A
19:52:00  node-1  Demoted node to passive ... node-2 / node-3 / node-1
19:52:01  node-1  ACTIVE_CHECK: 3 nodes are Active ...; waiting for the coordinator
19:52:05  node-2  ACTIVE_CHECK: redistributing 150 orphaned floating IP(s)     ← still active-active
19:52:31  node-2  ACTIVE_CHECK: 3 nodes are Active, consolidating onto MC-LB-node-2  ← target B
19:52:31  node-2  ACTIVE_CHECK: demotion of extra Active node was rejected hostname=MC-LB-node-3
19:52:58  node-2  ACTIVE_CHECK: demoted extra Active node hostname=MC-LB-node-1
19:52:59  node-2  ACTIVE_CHECK: demoted extra Active node hostname=MC-LB-node-4  ← undoes target A
```

node-1 consolidated onto node-4; node-2 — the node the cluster actually agreed was
coordinator, as the other three all logged "waiting for the coordinator" — then
consolidated onto *itself* and demoted node-4. In active-passive an Active node's
expectation set is the whole group (`deriveExpectedIPs` leaves `assigned == nil`, so
there is no per-node restriction), so two consolidation targets means the entire
group up on two nodes. That is the `duplicated=201`.

The two deciders disagreed because **`SetMode` propagated the mode change and the
demotions it implies only after its IP work had finished, and as two separate
messages**: `broadcastFullConfigToPeers` sends config with no states, and
`BroadcastClusterState` sends an envelope-only payload with no config. So for over
thirty seconds the peers held none of the switch: node-2 was still in active-active,
still Active, still acting as active-active coordinator — hence it declaring 150
addresses orphaned at 19:52:05 — and its member map still showed four Actives with
active-active loads. `ConsolidationTarget` prefers the most-loaded Active, which is
exactly the input that differs between nodes mid-rebalance, so node-1 and node-2
computed different answers from their different maps. node-1's own demotions were
undone within a second, which is why it was reporting three Actives at 19:52:01.

Two things ruled out along the way, so they need not be re-examined:
- **Not a view-stability problem.** Giving `enforceSingleActive` the
  `checksWithoutChange < viewStableCycles` guard that `reconcileActiveActive`
  already has would not have prevented this: node-2's competing consolidation fired
  31 seconds after the switch, far outside any 3-cycle window.
- ~~**Not the monitor re-widening its expectations.** The per-tick recompute at
  `ip_monitor_linux.go:230` is gated on active-active, so in active-passive the
  cached set is used and a demoted peer's stale expectation is its old ~50, not the
  whole group. The whole-group claim comes from the consolidation path only.~~
  **Wrong — disproved by the run-15 journals below.** Checking only the gate at
  `ip_monitor_linux.go:230` was too narrow. That gate guards the *per-tick* recompute,
  but the config-update path refreshes the monitor's expectations by a different route,
  and it widens them to the whole group the moment the mode becomes active-passive
  while the node still reads its own status as Active. The monitor is not the only
  source of a whole-group claim, but it is *a* source, and on run 15 it was the
  dominant one.

**Fixed:** `SetMode` now sends the config and the member states in a single
ConfigSync, before any address moves. `ConfigSync` already recognises a full config
by its `pulseha` root and reads `member_states`/`epoch`/`leader_id` off the same
object, so one payload carries both and no proto change was needed. Peers therefore
leave active-active and learn their new status together, which stops a second node
acting as coordinator at all. Demoted peers also now release from their own IP
monitors as soon as the message lands, rather than waiting on the serial
`BringDownIPs` calls. Unit test: `TestConfigAndStatePayloadCarriesModeAndStatesTogether`.

#### Verified live, run 15 (2026-07-27 22:56) — partially fixed

Binary `e56c22289697` on all four nodes (md5 verified), reached by a rolling restart
(passives first, Active last) which again preserved a clean `205/205 duplicated=0`
baseline — no defect #23. Switch to active-active converged to `51/52/51/51`,
`placements=205 unique=205 duplicated=0`, stable across three samples. TC-8 was then
issued **from node-1, a non-coordinator** (node IDs make `049-…`/node-2 the
coordinator), which is the configuration that produced run 14.

Measured with a 2s-resolution `ip addr` sampler running on all four nodes across the
switch, bucketed into 2s bins and de-duplicated per (bin, node, address):

```
  t(rel)  n1   n2    n3   n4    placements  unique  duplicated
   -1      51   52    51   51      205       205        0
   +1       1  121     1  142      265       149      116
   +3       1  185     1  202      389       205      184
   +5..+19  1  202     1  202      406       205      201
   +21      1  202     1    1      205       205        0
```

**TC-8's stated pass criterion is met**: from t+21 onward exactly one Active (node-2)
holds all 201 group addresses, the other three hold none, `205/205 duplicated=0`,
cluster online — stable across every sample for the following 8 minutes.

**The transient is reduced, not eliminated.** The two-Active window fell from ~90-100s
(run 14) to **~20s**, but the peak is unchanged: the whole 201-address group is still up
on two nodes for the duration. There is also a new, previously unmeasured **~2s window
where 56 addresses are on no node at all** (`unique=149` at t+1) — the per-address
down-then-up gap, visible here only because this run sampled at 2s.

What the fix did achieve: the two deciders now **agree on the target**. node-1 logged
`Consolidating floating IPs onto ... hostname=MC-LB-node-2`, i.e. it picked the
coordinator, not a third node as in run 14 — so the "two consolidations onto two
different targets" mechanism is genuinely gone, and node-1 and node-3 released
immediately (both at 1 address by t+1).

What still breaks, from node-4's journal — all of it inside 35ms:

```
22:56:33.052  node-4  Reconfiguring PulseHA server...              ← mode now active-passive
22:56:33.075  node-4  RPC BringDownIP on iface enX0 for 50 IP(s)   ← node-1's demotion lands
22:56:33.078  node-4  ENFORCE: Current node status ... status=Active
22:56:33.087  node-4  ENFORCE: Node is Active, ensuring expected IPs are present
22:56:33.088  node-4  ENFORCE: Bringing up missing IPs on Active node ...
```

node-4 applies the new mode, but its IP monitor still reads **its own status as Active**,
and in active-passive an Active node's expectation set is the whole group. So its
expectations widen from its 51-address share to all 201, and the ENFORCE loop re-adds
every address the incoming demotion strips — racing it to 202. It only backs off ~20s
later when its status actually changes.

The combined ConfigSync carries `member_states` saying node-4 is Passive, but node-4 did
not apply it. ~~A peer's opinion of *the local node's own status* is deliberately not
applied (the rule that also protects the maintenance flag), so node-4 cannot learn it is
Passive from the message that tells it the mode changed.~~ **Wrong — that rule has an
escape hatch for exactly this case, and the escape hatch was broken. See "Root cause
(defect #28)" below.**

#### Root cause (defect #28) — `decisive` was structurally always false

The rule is not "a peer never overrides the local node's own status", it is "an
*equal-epoch* peer never does". An equal epoch is a heartbeat and has no authority over
what a node knows about itself; a real demotion — election, mode switch, explicit
promote — arrives at a strictly higher epoch and is meant to apply. `SetMode` bumps the
epoch by 2 precisely so its demotions carry that authority.

`ConfigSync` computed that distinction against an epoch it had already overwritten:

```go
// full-config branch, ~line 4256
if incomingEpoch > s.clusterEpoch {
    s.clusterEpoch = incomingEpoch          // ← incoming epoch adopted here
    s.leaderID = incomingLeaderID
}
...
currentEpoch := s.clusterEpoch               // ← == incomingEpoch
decisive := incomingEpoch > currentEpoch     // ← can never be true
```

The envelope-only branch adopts the epoch the same way, so `decisive` was false on
**both** paths, for **every** payload: if the incoming epoch was higher it had just been
installed, and if it was not higher the comparison failed anyway. The guard at
`if !decisive && st != m.Status { continue }` therefore discarded every peer's view of
the local node's own status unconditionally, real demotions included. The comment
promising that "a real demotion ... always arrives at a higher epoch and still applies"
described intended behaviour that the code could not reach.

That is why node-4 held `mode=active-passive` and `self=Active` at once: it took the mode
off the config it had just saved and threw away the status that came in the same message.
Everything downstream follows from those two beliefs — an Active node in active-passive
expects the whole group, so its expectations widen from its 51-address share to all 201.

**Fixed:** snapshot the epoch on entry to `ConfigSync`, before either branch can adopt
the incoming one, and compare against that. Tests:
`TestDecisiveConfigSyncDemotesTheLocalNode` (a higher-epoch sync demotes the local node)
and `TestEqualEpochConfigSyncDoesNotDemoteTheLocalNode` (the defect #2 rule still holds —
this one passed before the fix too, so it pins the behaviour the fix must not invert).

**Also fixed, surfaced by that test under `-race`:** `ConfigSync` wrote `m.Status`,
`m.ActiveIPs` and `m.LoadFactor` while holding no member lock, and the post-load VIP
reconcile in `loadInitialMembers` read `member.Status` bare. Both now take the member
lock. This was a live race on the exact field the switch turns on.

**Not fixed — new defect #30, found in the same goroutine.** That post-load reconcile
brings up **every** group IP when it reads the local node as Active, with no
active-active filtering, 500ms after every full ConfigSync. It is a third whole-group
claimant alongside the monitor's enforce loop and the consolidation path, and it is
mode-blind in the way `expectedIfaceIPs` and `deriveExpectedIPs` were fixed not to be
(defects #2/#26 — this site was missed). In active-active `releaseUnassignedIPs` undoes
it within a tick, which is why TC-6 still passes, but it is a likely source of the
residual 5–14 steady-state duplication.

Second thing watched on this run and **not** observed: peers receiving a full config
mid-switch do restart their gRPC listener, but no `BringDownIPs` failure resulted —
releases were confirmed against `ip addr`, and node-1/node-3 reached zero immediately.

The deeper fragility is untouched and still worth its own entry: **`SetMode`
consolidates from whichever node received the CLI request, which need not be the
coordinator, while `enforceSingleActive` restricts the same decision to the
coordinator.** Two deciders for one invariant will disagree again whenever their
member maps differ. Making the switch delegate consolidation to the coordinator
would collapse them into one, and is the better long-term shape.

---

## Result 2026-07-28 (run 16) — TC-8 PASSES; defect #28 verified fixed live

Commit `427a456` (HEAD of `active-active-mode-join`, containing the #28 fix `2ae8189`),
built linux/amd64 and deployed to all four whitecrane nodes, binary md5
`9d7bf920cdd9c7b2bd3123911ee619cd` verified identical on every node. Rolling restart,
passives first and the Active last — the 201-address baseline survived intact, including
across the Active's own restart, so no #23 contamination.

### Method

A node-local sampler (`sampler.py`, 1s cadence, run under `sudo` so it can also read the
on-disk mode) writes `ts / host / mode / address-set` to each node's own disk; the four
files are correlated afterwards and a bucket is only scored once all four nodes have
reported. This avoids both traps hit in earlier runs: SSH round-trip skew, and the shared
temp file that fabricated duplicates. Note `fs.protected_regular` stops root reopening a
`loadbalancer`-owned file in `/tmp` — the sampler must create its own output file.

### TC-6 forward switch (active-passive → active-active), for context

`RC=0` in 1.6s. Converged to **50/51/50/50, unique=201, duplicated=0 at t+68s** and held
it flat for the remaining 170s. Transient during convergence: duplication peaked at 150
over t+8..t+67, coverage gap peaked at 42 addresses over t+8..t+45. That transient is
defect #29 (break-before-make) on the forward path, unchanged and expected.

### TC-8 reverse switch (active-active → active-passive) — **PASS**

`RC=0` in 1.6s.

| t | node-1/2/3/4 | placements | unique | duplicated | down |
|---|---|---|---|---|---|
| -3 | 50 51 50 50 | 201 | 201 | 0 | 0 |
| +2 | 50 51 50 0 | 151 | 151 | 0 | 50 |
| +3 | 0 144 0 0 | 144 | 144 | 0 | 57 |
| +4 | 0 **201** 0 0 | 201 | 201 | **0** | 0 |
| … stable to +272 | 0 201 0 0 | 201 | 201 | 0 | 0 |

**`duplicated=0` across all 294 sampled seconds.** Peak duplication for the whole run was
zero. Final state confirmed independently by `pulsectl status` (one Active, three Passive)
and a fresh `ip addr` read (201/0/0/0).

**Defect #28 is fixed, verified live.** The ~20s `duplicated=201` window is gone, and
consolidation now completes in **4 seconds** rather than 30+. The mechanism is directly
visible in the journals: node-3 and node-4 each logged
`ConfigSync: LOCAL node demoted from Active, releasing floating IPs newStatus=Passive`.
That is exactly the branch which `2ae8189` resurrected — before the `preSyncEpoch`
snapshot, `decisive` was structurally always false and this line could never be reached.

### New finding — the listener-restart window is real, and it *did* fail a release RPC

Run 15 watched for this and did not see it; run 16 did. node-1, driving the switch, logged:

```
WARN Failed to release IPs from demoted node during mode switch hostname=MC-LB-node-4
     error="... dial tcp 10.200.0.124:9083: connect: connection refused"
WARN Failed to release IPs from demoted node during mode switch hostname=MC-LB-node-3
     error="... dial tcp 10.200.0.123:9083: connect: connection refused"
```

Neither daemon restarted (`NRestarts=0`, both up since the rolling restart 17 minutes
earlier). The refusal window comes from the ConfigSync itself: node-3's journal shows
`Reloading PulseHA config` → `Beginning initial member loading process...` →
`Reconfiguring PulseHA server...`, i.e. the gRPC listener is cycled while the peer is
mid-switch, so an RPC arriving in that window is refused.

**Why the run still passed, and why this matters.** The demoted nodes released their
addresses anyway — via their *own* `ConfigSync` demotion path, not via the RPC. So
correctness came from a redundant path while the primary one failed silently and was
never retried (the same shape as defects #13 and #21).

**This is very likely #28's proximate trigger.** Before `2ae8189`'s fix the self-demotion
branch was dead code, so a refused `BringDownIPs` left the peer holding the whole group
with nothing to correct it — which is precisely the observed #28 symptom of a node reading
`mode=active-passive` and `self=Active` at once. The dead `decisive` branch was the root
cause; the listener-restart window is what exercised it. Logged as **#31** below.

### Coverage gap unchanged (#29)

`unique` dipped to 144/201 — **57 addresses momentarily on no node — for ~2s** at t+2..t+3.
Run 15 measured 56 for ~2s. Consistent, and consistent with the scoping already recorded:
`SetMode`'s goroutine runs all demotions to completion before the activation, by explicit
design. #29 remains open and unchanged; the handover-vs-failover fix is still the shape.

### New defect

**#31 — a ConfigSync cycles the gRPC listener, refusing in-flight peer RPCs, and the
failed `BringDownIPs` is never retried.** Only a `Warn`. Observed twice in one switch.
Benign here solely because the demoted peer self-releases; any path where the peer does
*not* self-release would lose the operation entirely. Fix shape: either avoid tearing down
the listener on a config-only reload, or retry the release and verify it against
interface state the way `8ffc1c1` made release confirmation work for promotion.

---

## Fix 2026-07-28 — defect #30, and a race it exposed (`c50f027`)

**#30 fixed.** The one-shot VIP reconcile at the end of `loadInitialMembers` now
decides its claim set the same way every other seeding site does. The
whole-group expansion and the assigned-subset filter were pulled apart into
`snapshotVIPGroups` and `filterToAssigned`, and `expectedIfaceIPs` was rewritten
in terms of the latter — so there is now one implementation of "which of these
addresses are mine", rather than two that can drift. Drift is exactly how this
defect happened: the #2/#26 fix taught `expectedIfaceIPs` and `deriveExpectedIPs`
about active-active and missed this site.

The release direction was deliberately left whole-group. A node that has just
been demoted may be holding addresses it was never assigned — the point of the
pass is to leave it holding none, so narrowing the release set to its
assignments would strand exactly the addresses that most need dropping.

**Not yet verified live.** #30's signature is the residual 5–14 addresses of
steady-state duplication in active-active, so TC-6 with the run-16 sampler is
the test: if the fix works, `duplicated` should settle at 0 rather than
drifting in single digits, and the per-sync re-claim spike 500ms after each
ConfigSync should disappear.

### Defect #32 — `Reload()` unmarshals over the live `*Config` (fixed `291564f`)

Found while testing the above, and it is the more serious of the two.
`ConfigSync` spawns the post-load reconcile *and* `go s.Reconfigure()`, and
`Reconfigure` called `config.Reload()` — which is `json.Unmarshal` straight into
the shared `*Config` every other goroutine is reading. `Config` does carry a
`sync.Mutex`, but neither `Load`/`Reload` nor readers like `ClusterCheck`,
`GetLocalNodeUUID` or `s.config.Nodes[...]` take it. Under `-race` that is a data
race; in production it is a `concurrent map read and map write` fatal error.

**Fix.** `Reload()` returns a fresh `*Config` and leaves the receiver untouched;
`Server.Reconfigure` and main.go's SIGUSR2 handler swap their pointer, so a
goroutine still holding the old pointer keeps reading a consistent snapshot. This
is the shape `ConfigSync` already uses at `s.config = newConfig`. Signature
changed to `Reload() (*Config, error)`.

Two details that are load-bearing:

- The fresh config is a **deep** clone of the receiver with disk state loaded over
  it. Deep because `json.Unmarshal` reuses an existing map, unmarshals into the
  existing `*Node` behind a map value, and reuses a slice's backing array when
  capacity allows — so a shallow copy still writes the structures readers walk.
  Cloning rather than starting from `config.New()` preserves `Load()`'s semantics:
  absent keys keep their current value, and under `PULSEHA_TEST=true` the reload
  stays a genuine no-op, which `TestReconfigureConcurrent_NoBindRace` depends on.
- `Reconfigure` had to start calling `memberList.UpdateConfig`. The member list
  holds its own config pointer and the health checker reads the config through it,
  so swapping only the server's pointer leaves both stale. Verified safe:
  `UpdateConfig` is a pointer assignment plus a `Capacity` refresh, and no
  `Reconfigure` caller holds the member-list or server lock.

**Measurement — the important lesson.** The figure recorded here earlier
("22/40 races before, 3/40 after", from `TestEqualEpochConfigSync` +
`TestDecisiveConfigSync` under `-race`) **did not reproduce**. Those two are 0/40
at HEAD, and the whole `make testrace` set (`./internal/... ./cmd/...
./packages/...`) is **0/20 both before and after the fix**. The existing suite is
simply not a detector for this.

`packages/config/reload_race_test.go` (`TestReloadDoesNotRaceLiveReaders`) is —
8 reader goroutines against 200 `Reload()` calls with a real on-disk config:

| | races |
|---|---|
| at HEAD before the fix | 20 / 20 |
| with the fix | 0 / 20 |

Use that test, not `make testrace`, for anything in this area.

**Residual, deliberately not closed.** ~278 unsynchronized `s.config` pointer
reads in `internal/server`. The pointer swap is now a third writer alongside the
pre-existing `s.config = newConfig` (ConfigSync) and `s.config = cfg` (RESYNC), so
the *pointer* race predates this and is unchanged in kind — but the swap makes it
more frequent, since `Reconfigure` runs on every full ConfigSync. Reads now always
see a fully-built immutable config either way, which is why this was left alone;
closing it properly means an accessor or `atomic.Pointer` across all 278 sites.
`tests/unit` under `-race` (8 concurrent `Reconfigure()` goroutines) is 0/20, so
nothing detectable was introduced.

**Pre-existing bug spotted in passing, NOT fixed.** `Load()` holds `c.Lock()` via
defer and calls `migrateConfig()`, which calls `c.Save()`, which takes `c.Lock()`
again — a self-deadlock on a non-reentrant `sync.Mutex`. Near-unreachable in
practice: `New()` populates the syslog defaults before `Load()`, so
`migrateConfig`'s "all four syslog fields empty" condition is false unless a
config file explicitly contains empty strings for all four.

---

## Result 2026-07-28 (run 17) — TC-6 PASSES; defect #30 verified fixed live

Commit `291564f` (code at `065e2b0`), binary md5 `fa1935bff2d8`, md5-verified on all
four nodes. Rebuilt the `RealTest` group from scratch — the previous group had been
wiped by `lbClearRestart` after the run-16 restarts (defect #3, working as documented).

### Method

Baseline rebuilt to 201 addresses on the real `10.200.0.0/23` range
(`10.200.0.152-255` + `10.200.1.0-96`). Range re-verified free before claiming it:
ping sweep (0 responders), then `arping -D` from node-1 across all 200 with a positive
control on the gateway and two cluster nodes. **The ping sweep alone is not
sufficient** — the cluster nodes themselves do not answer ICMP, so a silent host would
be missed; `arping` is the test that matters. Equally, `arp -an` on a workstation that
watched an earlier run is a **false positive** for "address in use": 199 of the 200
resolved to node-2's MAC purely as stale cache from run 16, while node-2 held only
its own `.122` and `proxy_arp=0`.

Sampling was the run-16 node-local sampler at 1s for 700s (`sampler.py`/`analyse.py`),
one file per node, correlated afterwards, scoring a second only when all four reported.
691 of 708 buckets scored; the 17 partial ones are all the sampler start stagger at the
head of the run, not skipped mid-run events.

Rolling restart (passives first, Active last) onto the new binary, while the groups were
still empty — the cheapest moment to take the restart, and it sidesteps #23.

### TC-6 forward switch (active-passive → active-active) — **PASS**

`mode set` returned **RC=0 in 0.53s**. Mode landed on all four nodes, CLI and on-disk
in agreement — no #17 divergence.

| t+ | mode | n1 | n2 | n3 | n4 | placements | unique | dup |
|----|------|----|----|----|----|-----------|--------|-----|
| 0 | active-passive | 201 | 0 | 0 | 0 | 201 | 201 | 0 |
| 16 | active-active | 151 | 50 | 0 | 0 | 201 | 201 | 0 |
| 25 | active-active | 134 | 0 | 0 | 50 | 184 | 151 | **33** |
| 26-44 | active-active | 147 | 0→25 | 0→25 | 50 | 197→247 | 151→201 | **46** |
| 45 | active-active | 93 | 25 | 25 | 50 | 193 | 193 | 0 |
| 54-75 | active-active | 53 | 50 | 50 | 50 | 203 | 201 | **2** |
| **76-693** | active-active | **51** | **50** | **50** | **50** | **201** | **201** | **0** |

**Settled at t+76 and held for 617 consecutive seconds** — `51/50/50/50`, spread 1,
`placements=201 unique=201 duplicated=0`. That is TC-6's pass condition.

### Defect #30 — verified FIXED live

#30's signature was 5-14 addresses of residual steady-state duplication in
active-active, from the post-load VIP reconcile re-claiming the whole group 500ms after
every full ConfigSync. Both halves are gone:

- **Steady-state duplication is 0**, not 5-14, across 617 consecutive scored seconds.
- **No node ever re-claims the group.** After the switch node-1 only ever descends
  (201 → 197 → 151 → 147 → 93 → 76 → 53 → 51); after t+50 the only values it takes are
  `{51, 53, 76, 93, 147}`. The other three never exceed 50. Under #30 an Active node in
  active-active re-claimed all 201 on each ConfigSync; three config reloads happened
  during this run (one each on nodes 2/3/4) and none produced a claim spike.

Beware the numbering trap when re-checking this: each sample file's own `t0` differs by
up to 10s from the first *full* bucket, so a naive per-file `max(held after t+20)`
reports 201 for node-1 — those readings are the pre-switch baseline, not a re-claim.

### Defect #32 — consistent with the fix, but the live run is weak evidence

No fatals, no panics, no `concurrent map read and map write`, `NRestarts=0` on all four,
uptime spanning the whole run including the switch and three config reloads. That is the
expected outcome, but the absence of a race in a single run is not proof — the real
evidence remains `packages/config/reload_race_test.go` (20/20 fail before, 0/20 after).

### The convergence transient, newly visible at 1s resolution

Previous TC-6 runs sampled every 30s, which cannot see a 20s window. At 1s the
convergence is **not monotonic** and violates the literal "no IP on more than one node
in *any* sample" wording, though the settled state is clean:

- **A rebalance batch is dual-homed for ~20s.** node-4 brought up its 50 at t+25 while
  node-1 still held 46 of them until t+45. A second, smaller instance followed: node-1
  and node-3 shared 2 addresses from t+54 to t+75 (22s). The destination claims before
  the source releases — the *opposite* order from #29's break-before-make, so the two
  findings bound the problem from both sides: this path duplicates for 20s, #29's path
  gaps for 2s.
- **Assignments churn: a node releases a batch it had already claimed.** node-2 held 50
  at t+16, dropped to **0** at t+25, and only returned to 25 at t+33 and 50 at t+50.
  During that window 50 addresses were up nowhere (`unique=151/201`), which is where the
  worst coverage gap comes from — not from a handover gap.
- Worst duplication **46** (in 42 of 691 scored seconds); worst coverage gap **50** (in
  25 of 691).

### New defect #33 — batched GARP announces addresses the node does not hold

**173 `failed to GARP. exit status 2`** in one convergence (node-2: 74, node-3: 49,
node-4: 50, node-1: 0). Root cause confirmed by hand on node-2:

| `arping -U -c 5 -I enX0 <ip>` | exit |
|---|---|
| address the node holds | **0** |
| address the node does not hold | **2** |
| 40 in parallel, all held | **all 0** |

So it is not a resource or concurrency limit (which `SendGARPBatch`'s fan-out would
have made the obvious suspect) — every failure is an announcement for an address that
was not on the interface at announce time. Surrounding context confirms it: `IP monitor:
IP removed but node is not Active, NOT restoring ip=10.200.1.96` immediately precedes a
burst of them.

The announcement is harmless in itself, but the announce set being wider than what is
actually held is the same class of bug as #15 (announcing addresses that never moved)
and carries #11's real risk: if the set is stale, an address that *did* just come up can
be missing from a successful announcement, leaving neighbours pointing at the old owner.

### New defect #34 — the enforce loop retries releases of addresses it does not hold

**925** `ENFORCE: failed to release unassigned floating IP ... cannot assign requested
address` on node-1 alone (nodes 2/3/4: 35/3/7), roughly 18 attempts per address. The
release set is computed from a view that still lists addresses the node has already
lost, and each attempt fails at the kernel. Benign in effect — the address is already
gone, which is the desired end state — but it is 925 `ERRO` lines per mode switch, which
is exactly the noise that would hide a release that genuinely mattered. Same root shape
as #33: an operation computed against a set the node does not actually hold.

Related, and cheap to fix: node-4 received `RPC BringDownIP on iface enX0 for 201 IP(s)`
for a group it held **none** of, producing 201 consecutive error lines.

### Defect status confirmed this run

- **#31 did not recur** — `connection refused` count is **0** on all four nodes. The
  listener cycle still happens (`Reconfiguring PulseHA server` once each on nodes 2/3/4),
  so the window is still open; it simply did not coincide with an in-flight RPC. #31 is
  a race, not a certainty — absence in one run is not a fix.
- **#16 is actively discarding signal**: 5 `IP assignment failed but IP is now present`
  across nodes 2/3/4. During a run with a real 46-address dual-homed window, the kernel's
  duplicate report was swallowed 5 times. This is why #16 is a prerequisite for #29's
  make-before-break: the log cannot currently distinguish a deliberate handover overlap
  from a split-brain.
- **#5 reproduced in a new, sharper form.** 200 *serial* `add-ip` calls left node-3 with
  **196** — precisely the last four added (`10.200.1.92-95`) — and it did not self-heal
  after 30s+. The recorded form of #5 was divergence under *concurrent* mutation, with
  serial adds converging; this run shows a serial batch losing its tail on one node. One
  further serialized `add-ip` snapped all four to 201, as previously recorded.

### Harness notes

- `pgrep -f sampler.py` over SSH is a **false positive** — it matches the command line
  of the shell running the `pgrep` itself. Check `ps -eo pid,cmd | grep "[p]ython3
  /tmp/sampler.py"` and whether the file is still growing.
- Counting held addresses with `grep -cE` over whole `ip addr` lines over-counts by one
  in this /23: every line carries `brd 10.200.1.255`, which matches a `10.200.1.` pattern.
  Parse the address field.
- The mode key in `config.json` is `pulseha.mode` (not `pulse.mode`).
- SSH to these hosts must be forced to IPv4. The names resolve to AAAA records
  (`2a02:1648:3008:1:202::12x`) with no route, so plain `ssh node-N.whitecrane.io` fails
  with "No route to host" while the host is perfectly healthy. `deploy.sh` hardcodes its
  SSH options, so pass IPs: `DEPLOY_HOSTS='10.200.0.121 ... .124' ./deploy.sh`.
- Host keys differ between the hostname and IP entries in `known_hosts` after node
  rebuilds; the run used a helper that bypasses `known_hosts` deliberately.

---

## Result 2026-07-28 (run 18) — TC-7 first ever run: PARTIAL PASS

Same binary as run 17 (`fa1935bff2d8`), same 201-address `RealTest` group, starting from
run 17's settled `51/50/50/50` active-active baseline. Measured directly from `ip addr` on
each node rather than via the 1s sampler (see the harness note at the end).

| Phase | Action | Result | Verdict |
|-------|--------|--------|---------|
| 1 | `node capacity 10` on node-2, no other action | `51/50/50/50` unchanged after 150s | see #35 |
| 1b | same cap, then a mode round-trip to force re-placement | **n1=64 n2=10 n3=63 n4=64** | **PASS** |
| 2 | `node capacity 0` on node-2 | **`51/50/50/50` within 60s, unprompted** | **PASS** |
| 3 | all four capped at 40 (total 160 < 201), then a round-trip | **n1=81** n2=40 n3=40 n4=40 | **FAIL** — see #36 |

**Phase 1b passes all three of TC-7's placement criteria.** The capped node holds exactly
its limit, the remaining 191 addresses are spread 64/63/64 across the uncapped nodes —
balanced within 1 — and nothing is left unplaced while capacity is spare. `ipam.Distribute`
/ `HasCapacity` do their job once they are actually invoked.

**Phase 2 passes, and reveals the asymmetry.** Removing the cap needed no trigger at all:
the coordinator saw node-2 under-loaded and rebalanced it from 10 back to 50 within 60s.

### New defect #35 — lowering a capacity never triggers re-placement

Setting a cap is config-only. It propagated correctly to all four nodes' `config.json`
(`node-2=10`), but produced **one** log line cluster-wide (`Node capacity updated` on the
node that took the CLI request) and no rebalance, distribute, or eviction on any node.
node-2 sat at 50 against a cap of 10 indefinitely — 150s, then across a further 5 minutes
of other activity.

This is consistent with the documented intent that lowering a capacity does not evict
existing IPs, and it is arguably the safe default. But the effect is that a cap is
unenforced until some *other* event happens to trigger placement, so an operator who caps
a node has no way to tell whether the cap is in force. It is also asymmetric with phase 2,
where *raising* a cap rebalances within one cycle — the under-loaded branch of the
coordinator's reconcile fires, the over-loaded branch has no counterpart. Fix shape: either
a bounded drain when a cap is lowered below current holdings, or report the node as
over-capacity so the state is at least visible.

### New defect #36 — overflow silently violates a cap instead of being reported unplaced

With all four nodes capped at 40 — total capacity 160 against 201 addresses — the excess
did **not** come back as `unplaced`. All 41 surplus addresses were placed on node-1, which
ended at **81 against its own cap of 40**, while nodes 2/3/4 held exactly 40.

Coverage stayed at 201/201, so this is not an outage, and node-1 was the former sole Active
(the consolidation target) — the seeding path lets the incumbent keep everything it already
holds without testing its own cap. So capacity is enforced against nodes *receiving*
addresses and not against the node *already holding* them.

**It is entirely silent.** Across all four nodes there are **zero** log lines matching
`unplaced`, `capacity exceeded`, `no capacity` or `over capacity` for the whole window. The
TC-7 expectation that `ipam.Distribute` returns the overflow as `unplaced` and that those
addresses are "reported rather than silently dropped" is not met: they are neither dropped
nor reported, they are quietly over-committed onto one node. An operator capping every node
to protect them would get one node at double its limit and no indication of it.

Restoring all four caps to 0 recovered `51/50/50/50` cleanly within 90s.

### Harness note

The 1s sampler is unreliable across a `pkill`/restart cycle: `sudo rm -f` of the sample file
leaves the old process writing to the unlinked inode while the replacement writes a new one,
and a `tail -1` then reports a stale count that looks plausible. Two switches were briefly
mis-scored as "no change" this way before direct `ip addr` measurement corrected it. For
phase-boundary checks like TC-7 — where the question is the settled distribution, not a
sub-second transient — poll `ip addr` directly and skip the sampler. If the sampler is
needed, start it once for the whole run and never restart it mid-run.
