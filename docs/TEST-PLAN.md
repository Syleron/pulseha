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

Row statuses are maintained as of **2026-07-30**. Where a row and a dated narrative
section below disagree, the narrative is the record of what was actually observed;
the row is the summary. "Unconfirmed" means the code the defect described has since
been rewritten by another fix, but the defect itself was never re-tested — treat it
as neither fixed nor reproduced.

| Case | Defect | Status |
|------|--------|--------|
| TC-1, TC-8 | #1 two-Active in active-passive | **Fixed `5b1e6bf`, verified live.** `enforceSingleActive` runs every health-check cycle, coordinator-gated |
| TC-2 | #6 Active self-strips VIPs on Unknown | **Fixed, verified live.** `isDemotion(old, new)` — only Passive and Maintenance count; Unknown is left to the health checker |
| TC-3 | #5 config diverges under mutation | **Fixed `2af3b80`, verified live run 19 (2026-07-28).** All four configs were identical at every sample across a 43-minute mutation run in active-active — 200 back-to-back `add-ip` then 200 serial `remove-ip` — and identical at the end. The historic 200/189/192/193 signature did not occur. A single broadcaster goroutine coalesces concurrent mutations, the push is retried, and the coordinator re-broadcasts once a minute to repair a peer that outlasted the retries |
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
| TC-7 | capacity enforcement | **Run 18 (2026-07-28): PARTIAL PASS.** Placement itself is correct — cap node-2 at 10 plus a re-placement trigger gives `64/10/63/64`, and removing the cap rebalances back to `51/50/50/50` unprompted within 60s. Two defects either side of it: **#35** and **#36** below |
| TC-7 | #35 lowering a capacity never triggers re-placement | **Open, found run 18.** Config-only: the value reaches all four `config.json` files but produces one log line and no rebalance, so a node sits over its cap indefinitely. Matches the documented "lowering does not evict" intent, but leaves a cap silently unenforced, and is asymmetric with raising one |
| TC-7 | #36 overflow silently violates a cap instead of being reported unplaced | **Open, found run 18.** All four capped at 40 (160 capacity vs 201 addresses) put all 41 surplus on node-1 → **81 against its own cap of 40**, nodes 2/3/4 exactly 40. Capacity binds receiving nodes, not the node already holding the addresses. **Zero** `unplaced`/`capacity exceeded` log lines on any node — neither dropped nor reported |
| TC-8 | return to active-passive | **Passes (run 16, 2026-07-28)** via `mode set` as specified — `duplicated=0` across all 294 sampled seconds, consolidation in 4s |
| TC-8 | #27 reverse switch runs two whole-group consolidations onto two different targets | **Fixed, verified live (run 15)** — `SetMode` propagates mode + states in one ConfigSync before moving any address; both deciders now pick the same target. See "Root cause (defect #27)" below |
| TC-8 | #28 a demoted peer's own IP monitor re-claims the whole group | **Fixed `2ae8189`, verified live run 16 (2026-07-28).** TC-8 ran `duplicated=0` across all 294 sampled seconds and consolidated in 4s; node-3/node-4 logged the `ConfigSync: LOCAL node demoted from Active` line that the fix resurrected. Original diagnosis: The peer could hold `mode=active-passive` and `self=Active` because `ConfigSync` adopted the incoming epoch *before* testing whether the payload was decisive, so `decisive` was structurally always false and every peer's view of the local node's own status was discarded — including a real demotion. See "Root cause (defect #28)" below |
| TC-6, TC-8 | #30 the post-load VIP reconcile brings up the whole group, mode-blind | **Fixed `c50f027`, verified live run 17 (2026-07-28).** Steady-state duplication is **0 across 617 consecutive scored seconds** (was 5-14), and no node ever re-claims the group — node-1 only descends after the switch, the other three never exceed 50, across three config reloads. See "Result 2026-07-28 (run 17)" below. Original diagnosis: The claim set now goes through the same assigned-subset filter as every other seeding site (`filterToAssigned`, extracted so the two cannot diverge again); the release set stays whole-group on purpose. Original diagnosis: `loadInitialMembers` spawns a goroutine that 500ms later brings up every group IP if the local node reads Active, with no active-active filtering. It runs on every full ConfigSync, so in active-active each Active peer re-claims the whole group and the enforce loop's `releaseUnassignedIPs` has to undo it — a likely source of the residual 5–14 steady-state duplication |
| TC-8 | #32 `config.Reload()` unmarshals over the live `*Config` while readers are in flight | **Fixed `291564f`, not yet verified live.** `Reload()` now returns a fresh deep clone with disk state loaded over it and leaves the receiver alone; `Server.Reconfigure` and main.go's SIGUSR2 handler swap their pointer, and `Reconfigure` also calls `memberList.UpdateConfig` so the member list and health checker don't keep the stale pointer. Signature changed to `Reload() (*Config, error)`. Detector is `packages/config/reload_race_test.go` — **20/20 fail before, 20/20 pass after**. The earlier "22/40 before, 3/40 after" figure from the demotion tests **did not reproduce** and should not be used: those are 0/40 at HEAD and `make testrace` is 0/20 either side of the fix. **Residual, deliberately open:** ~278 unsynchronized `s.config` pointer reads in `internal/server` — the pointer race predates this (ConfigSync and RESYNC already swap it) but `Reconfigure` runs on every full ConfigSync, so the swap is now more frequent. Readers always see a fully-built immutable config, so closing it properly means an accessor or `atomic.Pointer` across all 278 sites |
| TC-6 | #33 batched GARP announces addresses the node does not hold | **Over-announcing half fixed `541111c`, VERIFIED FIXED LIVE run 30 (2026-08-03); under-announcing half fixed 2026-08-03, NOT YET VERIFIED LIVE.** `failed to GARP. exit status 2` fell from run 17's **173** to **1**, on a *larger* group (288 addresses vs 201) with more churn — and the positive control fired, so this is not an all-zeros pass: the new `BringUpIP: skipped announcing addresses this node no longer holds` Debug line appeared **7 times across all four nodes** and suppressed **342** announcements of addresses the interface had lost (`count=72 of=72` on node-1, `69 of=72` and `37 of=38` on node-4, `71 of=72` on node-3, `49 of=50` on node-2). The single surviving failure is the **documented unclosable residual** — node-4 announced 3 of its first 72 and lost one of those between `addressAbsentFrom` and its own `arping`, which is the check-to-syscall window the fix narrows rather than closes. `SendGARP` has **no callers outside the batch** (verified by grep at `541111c`), so nothing announces on a path this fix does not cover. Reproducer was run 17's shape: 288-address group settled `72/72/72/72`, consolidated to `0/288/0/0` via `cluster mode set --mode active-passive` (rc=0 in 29s), then back to active-active — during which node-3 went 72 → 19 → 7 → 17 → 60, which is the churn that makes an announce set stale. Converged `72/72/72/72`, `unique=288 dup=0`, stable 4 min. **The consolidation direction cannot test this** and scored 0/0: node-2 kept every address it announced, so there was nothing to go stale — a valid negative, and the reason the switch back is the test. **The residual half is now sharpened by live evidence:** the same window shows 12 `BringUpIP` RPCs requesting 559 address bring-ups for a 288-address group, and node-1's final 72 were placed by the **ENFORCE** path, which per #11 never announces — so an address can end up live under a holder that never announced it, which is #11/#15's risk; **that half is now fixed too, see the second fix below.** The announce set is the caller's *intent*, recorded per address as each one came up, and the batch then takes waves of seconds to work through it — `ceil(n/32)` waves at ~4s an arping, so the last wave of a 200-address group announces roughly half a minute after the set was built, with the enforce pass releasing addresses throughout. Run 17's own context line says exactly this: `IP monitor: IP removed but node is not Active, NOT restoring` immediately precedes a burst. **Fix: `sendGARPBatch` re-reads each address against the kernel immediately before its own arping and skips one the interface no longer holds**, reporting it as skipped rather than failed. The check sits *inside* the fan-out goroutine rather than filtering the set up front, because a pre-filter is read once and then acted on for as long as the batch takes — which is most of the window the defect lives in. The window cannot be closed, only narrowed to the syscall, the same reasoning `AddrAddSatisfied` records for #45. `addressAbsentFrom` answers a **definite negative only**: an address whose state cannot be read is announced anyway, because suppressing a legitimate announcement is the one way this fix could do harm — nothing re-announces on its own, so neighbours would hold the previous owner's MAC until their ARP entries age out. `SendGARPBatch` now returns `(skipped, err)` and **both callers report the skip on the daemon's logger**, not the network package's, whose package-level logger nothing ever calls `SetLevel` on — a Debug line there cannot reach the journal at any `logging_level`, so the skip would be unverifiable live (#61's lesson). Tests: `TestSendGARPBatchDoesNotAnnounceWhatTheInterfaceHasLost`, `TestAnAddressIsAnnouncedWhenItsAbsenceCannotBeProven` and `TestAddressAbsentFromReadsTheInterfaceItIsGiven` in `packages/network/network_test.go`; the first fails against the unskipped batch on all four of its assertions, and the third — which needs real netlink, so it **skips on macOS and was run on Linux in a container** — fails when the interface comparison is dropped. Simplification found by mutation-testing the mask handling: `parseTargetIP` already accepts CIDR form, so the `GetCIDR` step this fix first carried was redundant and was removed. **Residual of the first fix:** the `err != nil` arm (announce when absence cannot be proven) is not covered by a test — netlink does not fail on demand. **Live check for the first fix:** drive a convergence that moves addresses (run 17's shape) and expect `failed to GARP. exit status 2` at **0** on all four, with the new `skipped announcing addresses this node no longer holds` Debug line as the positive control — zero of both means the race did not occur, not that the fix worked. **Second fix (2026-08-03), the under-announcing half — the one that costs traffic rather than log lines.** Three paths could leave an address live and never announced, and the cause is one thing: *the announce set was the caller's success list*. `enforceExpectations` kept no list at all — it brought up every missing address with `network.BringIPup` and announced none of them, which is run 30's node-1 evidence above. `bringUpIPsLocally` and `Server.BringUpIP` appended to `upIPs` only after a bring-up they judged successful, so an address that came up *despite* a reported failure — #45's documented race, which `BringUpIP` already carries two rechecks for — was live and unannounced, and one that appeared after the last recheck was lost regardless. **Fix: the announce set is now every address the caller got as far as *attempting*, and the kernel decides at announce time.** That is safe only because of the first fix: `sendGARPBatch` already re-reads each address immediately before its own arping, so offering an address that never came up costs one netlink read and a skip, while offering one that came up late gets it announced. Nothing else could know — a list built during a placement loop is stale by the time the batch reaches its last wave, which is the same staleness the first fix addressed, pointed the other way. New `placeMissingFloatingIPs` in `internal/membership/ip_monitor.go` places a set and announces it in one batch, deliberately mirroring `releaseSurplusFloatingIPs` beside it (kernel operations injected, per-address outcomes returned for the caller to log). It announces unconditionally rather than only on success, so a bring-up that failed with `file exists` — another writer got there first, the common case on a converging cluster — is announced too. Announcement failures come back separately from placement failures: the addresses are up and serving either way. **Two paths are deliberately left silent, and the reasons are worth keeping.** `AddIPToGroup` (`server.go:3515`): a brand-new address has no stale ARP entry anywhere to correct, and #39's ~30ms-per-add depends on that add staying announcement-free. `restoreIP` (`ip_monitor_linux.go`): it runs **on the netlink watcher goroutine**, whose channel is `make(chan netlink.AddrUpdate, 32)` with no overflow handling (`ip_monitor_linux.go:16`), and an arping costs ~4s — so announcing there would drop address events during exactly the churn that produces them, and a dropped *removal* is an expected address never restored. That is #4/#8's failure re-introduced, and it is why every announcement in this codebase is batched off a loop like that one. The silence costs little there specifically: the address is taken off this node and goes straight back onto it, so neighbours' ARP entries still point at this node's MAC, whereas a placement that genuinely moves an address between nodes goes through the enforce pass or a bring-up RPC and now announces. An earlier draft of this fix did route `restoreIP` through `placeMissingFloatingIPs` and was reverted for this reason — the announcement was correct and the goroutine it would have run on was not. Tests: `internal/membership/ip_monitor_place_test.go`, six cases, mutation-tested against **both** pre-fix behaviours — reverting to "announce nothing" (the enforce pass as it stood) fails five of them, and reverting to "announce only what the bring-up reported as successful" (the two RPC callers as they stood) fails exactly the two that encode the #45 arms, which is the discrimination that matters: the second mutation is the subtler defect and only two assertions can see it. Log surface is preserved deliberately (the per-address `ENFORCE: About to bring up IP` line is kept by wrapping the injected bring-up) so run-30's grep-based method still applies. **Live check:** in the same run-17-shaped convergence, the addresses ENFORCE places must now be announced — expect `ENFORCE: Successfully brought up IP` to be followed by GARP activity for those addresses on that node, and `arping` counts on a node whose final share was ENFORCE-placed to be non-zero where run 30 showed the placement with no announcement. Watch `ENFORCE: failed to announce some placed IPs` and `ENFORCE: skipped announcing addresses this node no longer holds` — the latter should fire, since the attempted set now legitimately contains addresses the pass did not manage to place, and a run with zero of both plus zero GARP failures again proves only that nothing moved. Original diagnosis follows. **Found run 17.** 173 `failed to GARP. exit status 2` in one convergence (n2=74, n3=49, n4=50). Confirmed by hand: `arping -U` exits 0 on a held address, 2 on an unheld one, and 40 in parallel all succeed — so it is a stale announce set, not a fan-out limit. Carries #11's risk: an address that did come up can be missing from a successful announcement |
| TC-6 | #34 the enforce loop retries releases of addresses it does not hold | **Found run 17. The enforce-loop half is fixed with #41 and VERIFIED FIXED LIVE run 23 (zero error-level lines across a 71-address release storm); the `Server.BringDownIP` RPC half is still open and was not re-tested in run 23.** 925 `ENFORCE: failed to release unassigned floating IP ... cannot assign requested address` on node-1 in one switch, ~18 per address. Harmless in effect, but it is the noise that would hide a release that mattered. Same shape as #33. #41 turned out to be the mechanism behind these lines and the fix stops both the retry and the error-level log. Still open, and a different site: node-4 got `RPC BringDownIP for 201 IP(s)` for a group it held none of — `Server.BringDownIP` does not filter the request against what the node actually holds |
| TC-6 | #41 the release pass races its own IP inventory snapshot | **Fixed `573a3e6`, VERIFIED FIXED LIVE run 23 (2026-07-29).** Zero `ENFORCE: failed to release unassigned floating IP` on all four nodes across a 71-address release storm (95/96/0/96 → 72/72/71/72, transient `duplicated=14`), where run 22 had 27 on node-1 and 31 on node-2. The positive control fired: the Debug no-op classification appeared **201 times** (15/80/77/29), so the races genuinely happened and every one was classified rather than merely absent — an absence-of-errors run with a silent Debug counter would have proved nothing. Found run 22, pre-existing (27 lines on node-1 and 31 on node-2 on the *old* binary in the same window, so not introduced by `b6c2431` — but #40's widened release pass hits it more often). `ENFORCE: failed to release unassigned floating IP ... cannot assign requested address`: the address moved between the inventory snapshot and the bring-down, so the release is a no-op logged at **error** level. Harmless to coverage — run 22 settled exact — purely misleading logs, and the same class as #33/#34. Root cause: `enforceExpectations` builds one `BuildIPInventory()` snapshot at the top of the tick and passes it all the way down to `releaseUnassignedIPs`, but the Active branch's bring-up loop runs *between* them, so the snapshot that chooses the surplus set can be seconds and N address moves old. **Fix, two parts.** The release pass now builds its **own** fresh inventory instead of inheriting the stale one, which is what stops it attempting releases of addresses that have already gone; and each address is re-checked live immediately before the bring-down, with a failed bring-down re-checked again afterwards — an address that is gone by then lost the residual race, which cannot be closed, only classified, so it logs at Debug as a no-op instead of Error. A failure on an address the node *still holds* is still an error, which is the line that was worth reading all along. Tests: `internal/membership/ip_monitor_release_test.go` (each of the two checks mutation-tested — removing either makes a test fail). **Live decomposition (run 23):** of the 201 addresses classified as no-ops, **31** had actually reached the kernel and lost the residual check-to-syscall race — countable separately as `NETWORK: Unable to bring down IP … cannot assign requested address` at **Warn** from inside `BringIPdown` (15 node-2, 9 node-3, 5 node-4, 2 node-1) — and the other **170** were skipped before the syscall by the fresh inventory plus the live pre-check. Both halves of the fix are therefore separately observable live. **Residual:** those 31 Warn lines come from `packages/network`, one layer below the fix, which was deliberately not touched — error-level noise for a no-op is gone, warn-level noise for the same no-op is not |
| TC-6 | rebalance dual-homes a batch for ~20s | **Open, found run 17.** Destination brings up before source releases: n1&n4 shared 46 addresses for 20s, n1&n3 shared 2 for 22s. The opposite order from #29, so the two bound the problem from both sides. Also non-monotonic: node-2 released a batch it had already claimed (50 → 0 → 25 → 50), putting 50 addresses on no node for ~8s. Only visible at 1s sampling; earlier 30s sampling could not see it |
| TC-8 | #29 per-address down-then-up gap during consolidation | **Open** — run 15 measured `unique=149/205` for ~2s at t+1: 56 addresses momentarily on no node. Run 16 reproduced it at 57 for ~2s. Not per-address: `SetMode`'s goroutine runs all demotions to completion before the activation, by design. Fix shape is handover (make-before-break) vs failover |
| TC-3 | #37 `AddIPToGroup` brings each new address up on every assigned node, serially, with inline per-IP GARP | **Open, found run 19.** ~13s per `add-ip` in a four-node cluster (200 adds = 43 min): node-1 sends `BringUpIP` to each peer in turn, waiting ~4s on each, then brings it up locally. Pre-existing (`4f09169`, 2025-03-17), not a regression. Mode-blind — in active-active a new address is momentarily live on all four before the enforce loop releases three. #4's `SendGARPBatch` covered the failover paths, not this one. **Partly addressed by the #39 fix, that part VERIFIED LIVE run 23:** the fan-out is now concurrent and off the request path, so an `add-ip` returns as soon as the config is committed rather than after ~4s × peers — measured at 0.034–0.088s per add against ~13s (all four up) and ~28s (one peer down). The remaining cost is unchanged — each peer still announces per-IP inline, and the fan-out is still mode-blind — it just no longer bounds the caller. **Run 23 refinement: in a burst the fan-out is now almost entirely ineffective.** With all four nodes healthy, **56 of 60** peer `BringUpIP` RPCs were refused (19 node-2, 19 node-3, 18 node-4), all `Unavailable … connection refused` — the peers' listeners are cycling under the config-sync storm the adds themselves cause (#31), and the fan-out now fires *into* that window because it runs after the commit instead of before it. Harmless only because each peer's ENFORCE pass converges on the broadcast config, which is exactly the guarantee the #39 fix leans on — and which #43 shows is not unconditional |
| TC-3 | #38 an add reported successful is erased from every node | **Fixed `e2c7143`, VERIFIED LIVE run 20 (2026-07-28).** 44 adds issued across three batches, each with node-2 (the coordinator) deliberately made to fall behind mid-batch — 0 losses, all four configs identical at 245, every added address present on every node. The guard was observed rejecting a behind coordinator on the wire, which is the first live confirmation of the mechanism: node-2 re-broadcast at `version=44` after a restart and all three peers replied superseded (`peer holds a newer config … version=44`), while node-3 logged `ignoring superseded config sender=049… version=20 held=20`. Original finding, run 19: 9 of 200 adds ended absent from all four configs, the identical set on each — *uniform loss, not divergence*, so TC-3's "all four agree" criterion cannot detect it and scored that run a pass. Root cause is the #5 fix's per-sender generation: it ordered a sender's snapshots against that sender's own previous ones and nothing else, so it was structurally blind to a peer that was simply **behind**. The coordinator re-broadcasts once a minute and is not the node taking the mutations, so its stale view arrived on a sequence of its own and was applied wholesale. Made certain by a second mechanism: the counter only moved on a node's *own* mutations, so a coordinator that had never mutated stayed at 0 and broadcast **unversioned**, which the receiver applies unconditionally — run 19 caught exactly that on the wire (node-2 pushing at `generation=0` inside the window where `.181`/`.182` were lost). Replaced by a Lamport clock over the config's *content*: a mutation sets it one above everything seen, applying a peer's config adopts that config's version, and a lower version is stale whoever sent it. Ties broken on node ID so concurrent mutations converge instead of each rejecting the other. The `2af3b80` safety argument — one speaker per cluster — guaranteed only that one node could corrupt the cluster, not that none would |
| TC-3 | #39 an `add-ip` that returns rc=1 has still been applied | **Fixed `bef7286`, VERIFIED FIXED LIVE run 23 (2026-07-29).** 40 adds over two 20-add bursts, **every one rc=0** in **0.034–0.088s**: one burst with node-4 stopped (12s for all 20, against ~28s *per add* pre-fix) and one with all four healthy (4s for all 20, against ~13s per add). No add reported a failure for work that was applied, and the stopped node picked the additions up from config within a minute of restart. **But the fix removes the accidental 13–28s-per-add rate limit, and that exposes #43** — the commit now lands so fast that the config broadcast loses the race and the change stays local, turning #39's false *failure* into a false *success*. #39 itself is fixed; read it together with #43 before calling `add-ip` trustworthy. Original finding, run 20: `10.200.1.105` and `.112` exited 1 with `DeadlineExceeded … RST_STREAM` yet were present in the configured group on all four nodes afterwards (221, not 219). The caller's deadline expires while the mutation has already been applied and broadcast server-side. Same family as #13/#21/#31 — the returned status is not evidence of what happened — but inverted: a failure is reported for work that succeeded, so a non-zero add cannot be excluded from an expected count. Reachable because #37 makes an add take ~13s normally and ~28s with one peer unreachable. Root cause, from the code: `Client.Send` puts a 30s deadline on every CLI call and `group add-ip` sets none of its own, while `AddIPToGroup` ran the whole bring-up fan-out **before** it appended to the config and never checked `ctx` — so the deadline fired against ~2s of margin and the handler carried on to `Save()` and `markConfigDirty()`. **Fix: commit first, announce second.** The append + `Save()` + `markConfigDirty()` now run before any interface work, so the returned status describes the committed state; the peer fan-out moved off the request path into `bringUpGroupIPOnPeers` — concurrent, on its own 30s context rather than the caller's, best-effort because each peer's ENFORCE pass converges on the broadcast config anyway. Checking `ctx` instead would be worse: aborting mid-fan-out leaves the address up on some nodes and absent from the config. The "failed to bring up on any node" fatal branch is gone with it — an address that came up nowhere is now a committed config plus warnings, which is what the enforce loop is for. Tests: `internal/server/add_ip_test.go` (`TestAddIPToGroupCommitsWhenNoInterfaceComesUp`, `TestAddIPToGroupDoesNotWaitForTheRemoteFanOut` — the second returns in 10.003s at HEAD, i.e. exactly the caller's deadline, and 0.00s with the fix) |
| TC-8 | #31 a ConfigSync cycles the gRPC listener; the refused `BringDownIPs` is never retried | **Open, found run 16, reconfirmed live run 23 on a healthy four-node cluster** — 56 of 60 peer `BringUpIP` RPCs and every retry of the config broadcast itself were refused during a 20-add burst, with only 1–2 `Reconfiguring PulseHA server` events per peer in the window, so one teardown refuses many seconds of traffic. It is the amplifier under both #37's dead fan-out and #43's lost propagation. **Original finding:** node-1's release RPCs to node-3 and node-4 both got `connection refused` mid-switch although neither daemon restarted — `Reconfiguring PulseHA server` tears the listener down. Only a `Warn`; nothing retries. Benign only because the demoted peer self-releases via its own ConfigSync, which is the path `2ae8189` restored — so this is very likely #28's proximate trigger. **Fixed at the cause, NOT verified live: `Reconfigure` now rebinds the cluster listener only when the bind address actually moved.** Every full `ConfigSync` spawns `Reconfigure`, which unconditionally `GracefulStop`ped the listener and bound a fresh one — for a config change that never touched the bind address, which is what a group edit or a peer status change is. The gRPC server here is built with no credentials and no options, so the address is the whole of the listener's configuration and an unchanged address means there is nothing to re-apply. The serving goroutine clears the record when `Serve` returns, so a listener that dies on its own is still rebound by the next `Reconfigure` rather than skipped as still-serving. Does not by itself fix #43 — a genuinely down peer still misses a broadcast — which is why the retry above is the other half. Tests: `TestReconfigureKeepsTheListenerWhenTheBindAddressIsUnchanged` (fails against HEAD) and `TestReconfigureRebindsWhenTheBindAddressChanges` (passes at HEAD; it guards the skip from getting too wide) |
| TC-3 | #42 `pulsectl config set` reports success but does not apply cluster-wide | **Fixed, NOT verified live (2026-07-30).** `UpdateConfig` now knows the scope of every key it accepts. **Cluster-wide keys** (`hcs_interval`, `fos_interval`, `fo_limit`, `auto_failback`) are stamped and broadcast through `markConfigDirty()` exactly like a group mutation, so they inherit #43's retry as well as the broadcast. **`mode` is delegated to `SetMode`** rather than written into the config: changing the mode consolidates or spreads the floating IPs and re-broadcasts the member statuses that belong with it, and writing the value past all of that is precisely what produced the 529 `4 nodes are Active in active-passive mode` lines. The delegation happens *before* `s.Lock()` is taken, because `SetMode` takes it and the lock is not reentrant — the recurring shape behind #32/#46/#55. `SetMode` additionally marks the config dirty now, so the mode has the retrying broadcaster behind it instead of one unretried pass per peer whose result was discarded. **Logging and syslog keys are node-local by design** — `ConfigSync` preserves them so a peer left at debug for an investigation survives the next broadcast — so they are applied locally and *reported* as node-local; the CLI prints the reach of every change and the help text lists which keys are which. Keys not in the table (notably `local_node` and `cluster_token`) are now refused rather than written. Two prerequisites fixed underneath: `Config.UpdateValue` rolls back a value the validator rejects (it used to leave it in the live struct, failing every subsequent `Save()` and standing ready to be broadcast by the next successful mutation) and its error now names the constraint; and #55 below, found while testing this. Tests: `internal/server/config_scope_test.go` and `packages/config/update_value_test.go`, all seven observed to fail against `d4408e9` in a throwaway worktree — the mode test reporting `2 members Active … want exactly 1`, which is run 23's wedged state in miniature. **Original finding, run 23:** `config set mode active-passive` on node-1 printed `Successfully updated mode to active-passive` and rc=0, but **only node-1's config changed** — nodes 2, 3 and 4 still read `active-active`. node-1 then logged `ACTIVE_CHECK: 4 nodes are Active in active-passive mode; waiting for the coordinator to consolidate` **529 times** while the coordinator, still in active-active, never consolidated: a wedged cluster with zero placement change and a success message. The same command's help says "Set a configuration value and apply it to the cluster". Also seen with `logging_level`: one invocation reached node-1 and node-2 only, so `logging_level` had to be set on each node individually (both to enable Debug for this run and to restore `info` afterwards). Distinct from #43 — this is a *single* quiet mutation, not a burst, and the `pulseha` section rather than `floating_ip_groups`. Severity is high for `mode`: a two-mode cluster is exactly the split-brain configuration quorum is meant to prevent, and the operator is told it worked |
| TC-3 | #43 a burst of config mutations commits locally and never propagates; the reconcile does not repair it | **Second fix 2026-08-03, NOT verified live — the retry (2026-07-30, below) was necessary but not sufficient, and the missing half was on the *receiver*. `ConfigSync` could not represent a removal at all.** Its merge read "the incoming list is missing or empty" as "this sender has no opinion, keep mine" — but absence and emptiness are exactly what a removal looks like on the wire, so all three removing mutations were undone by every peer that received them: `commitGroupDeletion` deletes the key, `UnassignGroupFromNode` deletes an interface entry once it has no groups left, and `RemoveIPFromGroup` leaves the group present with an empty list. The receiver then replied `Success: true`, so the sender's broadcaster recorded full propagation and cleared the retry state — **the 2026-07-30 retry could never fire for this arm, because nothing ever reported a failure.** That is why run 27 and run 28 kept reconfirming #43 on binaries that already contained the retry: write 1 of a delete (the assignment drop) changes lists and propagates, write 2 (the delete) is a removal and is refused silently by every peer. **Fix: within the full-config branch a payload that *carries* the field is authoritative about it, including about what is no longer in it.** Keyed on the map being **nil** — the field absent from the JSON or explicitly null — rather than on it being empty, which is what preserves the case the merge was actually written for: `floating_ip_groups`/`group_assignments` carry no `omitempty` and `config.New`/`Load` always initialise the maps, so any live daemon sending a full config emits at least `{}` and "I have no groups" stays distinguishable from "I do not speak groups". Safe to honour absence because this point is past two stamp checks, the second under `s.Lock()`: the payload is strictly newer than what the receiver holds, so a removal is ordered on #38's Lamport clock exactly like a content change, and the wholesale semantics are what a newer config already had for a group's address list. **Node-level absence is deliberately left alone** — a node missing from an incoming config is still kept, so this does not make a stale payload able to delete peers, and `leave`/`RemoveMember` propagation is not part of this fix. Tests: `internal/server/config_deletion_test.go` — `TestConfigSyncAppliesAGroupDeletion`, `TestConfigSyncAppliesAGroupUnassignment` and `TestConfigSyncAppliesEmptyingAGroup` all observed to fail against the unfixed merge (the group came back, the unassignment was undone, and all addresses were restored), with `TestConfigSyncWithNoGroupsFieldPreservesLocalGroups` passing both before and after as the guard on the nil case. Adding those tests exposed a **latent flake in this package's harness**, now fixed: `ConfigSync` replies before the `go s.Reconfigure()` it spawns has re-read the config file, and every harness here points the package-level `config.CONFIG_LOCATION` at its own `t.TempDir()`, so a test that ended while that goroutine was still inside `config.Load()` let the next test's setup write the global it was reading — a real data race, in the harness rather than the daemon, hit by roughly 1 run in 6 of `make testrace` and 0 of 12 after (`onAsyncReconfigure`, a nil-in-production test seam; waiting on the config pointer instead cannot work, because the pointer Reconfigure swaps to is indistinguishable from the one `ConfigSync` installed when the goroutine wins). **First fix 2026-07-30 (the retry), also not verified live; reconfirmed live on runs 27 and 28, whose binaries `0c2ad56`/`671ec04` already contained it. Found run 23. Mirror of #39 and made reachable by its fix.** Before chasing this further, read #60: a propagating delete is what turns #60's restored share into a permanent strand, which is why #60 was fixed first (2026-08-03, itself awaiting live verification). **Fix: the node that owns the change is the node that retries it.** A broadcast that exhausts its four in-pass attempts now records the version and the outstanding peers, and the broadcaster re-pushes on its own timer — 5s, doubling to a 60s ceiling — until every peer accepts or a newer mutation supersedes it. No coordinator involvement, which is the whole point: the repair is owned by the node holding the unpropagated config. Safe for a non-coordinator to push because #38's Lamport stamp already guards the receiver: a peer holding something newer answers `superseded config version ignored`, which drops it from the retry set rather than overwriting it — that guard is what made the original coordinator-only gate unnecessary. The retry re-snapshots and pushes to *every* peer rather than only the outstanding ones, since by then this node's config may have moved on again; a peer already holding the version ignores the message. Deliberately **not** fixed by blocking the `add-ip` reply on propagation — that would undo `bef7286` and put #39 and #37's latency straight back. Paired with the #31 fix below, which removes the condition that made the in-pass attempts fail in the first place. Tests: `internal/server/config_propagation_test.go` — `TestUnpropagatedConfigIsRetriedUntilEveryPeerAccepts` (against HEAD: 4 refused pushes, 0 accepted, forever) and `TestPropagationRetryBacksOffAndStops` (the schedule has a ceiling and stops) 20 `add-ip` calls in 4s from node-1 (all rc=0): node-1 reached 286 while node-2/3/4 stayed at 267/268/268, **stable for 135s+** with all four healthy and no node down. Reproduced twice (the first burst, with node-4 stopped, left node-1 at 265 against 246/251, stable 3min+). Mechanism, all on the wire: the per-add broadcasts coalesce correctly (`CONFIG_BROADCAST: superseded by a newer broadcast, abandoning retries`), but every retry of the *final* broadcast is refused (#31), ending in `CONFIG_BROADCAST: peers did not accept the config after all retries; waiting for the periodic reconcile version=N peers=[all three]` — and **that reconcile only runs on the coordinator**: node-1 logged **0** `CONFIG_RECONCILE` lines in 4–5 minutes while node-2 logged 4. So a failed broadcast from a *non-coordinator* is never repaired; node-2's own reconcile pushes its older config, node-1 correctly rejects it as stale (#38's guard working), and nothing pulls node-1's newer config out. Repaired only by the next *successful* mutation, which carries the whole backlog: a single quiet `add-ip` took node-2 from 246 → 266 and node-3 from 251 → 266 in one push. **Consequence: `add-ip` returns success for a change that exists on one node only, for an unbounded time.** Pre-existing in mechanism (coordinator-only reconcile + #31), but before `bef7286` each add took 13–28s and the broadcasts were naturally spaced far enough apart to succeed — runs 19 and 20 landed 244 adds this way. Fix shape: let any node reconcile its own unpropagated version, or block the `add-ip` reply on propagation rather than on the local commit |
| TC-6 | #44 `unassign` alone does not trigger the reclaim; only a restart of the unassigned node does | **NO LONGER REPRODUCES, run 29 (2026-08-03) — fixed as a side effect of #58 (`6d55cd8`), not by anything aimed at #44.** `unassign` from node-4 on the coordinator: released, and nodes 1/2/3 reclaimed all 9 within 15s, settled `12/12/12/0` with the cluster total still 36 of 36, held 3 min, no restart. The reclaim was never broken — the orphan detector builds `hosted` from each member's `ActiveIPs`, and before #58 a released address was never dropped from its own node's list, so `orphanedGroupIPs` saw nothing orphaned and correctly declined to act on a set it could not see. Original finding, run 23: `group unassign RealTest` from node-3 (rc=0, propagated to **all four** nodes' `group_assignments` within seconds) made node-3 correctly release all 71 of its addresses within 6s — and then the cluster sat at `n1=72 n2=72 n3=0 n4=72, placements=216, missing=71` for **8 minutes**. 71 configured addresses on no node at all, with every node agreeing on the config. node-1's `ENFORCE: Current expectations` never widened to include them and there were **zero** `ACTIVE_CHECK: rebalancing`, reclaim, vote or capacity lines on node-1 or node-2 in the window — the reclaim never even tried. Restarting node-3 with the group still unassigned then reclaimed all 71 immediately (`95/96/0/96`, `unique=287 missing=0`), which is why **run 22 did not see this**: it unassigned and restarted in the same breath, so the restart-driven reclaim masked the missing unassign-driven one. Distinct from #40 (the release half, which works) and from #13 (dropped RPCs — here no RPC is attempted) |
| TC-6 | #45 the bring-up path logs an already-present address as an error — #41's mirror | **Fixed 2026-07-30, VERIFIED FIXED LIVE run 25 (2026-07-31) — one of the two arms exercised live.** Across a full run covering five daemon restarts and two make-before-break storms (transient `duplicated=5` then `duplicated=27`), on all four nodes: **0** `IP monitor restore: failed to add addr`, **0** `NETWORK: netlink.AddrAdd failed`, **0** `ENFORCE: Failed to bring up IP on Active node`, and **0** occurrences of the escalation's cause string `unable to bring IP up as netlink failed to do so` — where run 23 had 22, 9 and 15 respectively. The positive control fired: the Debug classification `IP monitor restore: expected IP was already back` appeared **48 times** (n1=7, n2=9, n3=22, n4=10), so the EEXIST races genuinely happened and were classified rather than merely absent. The decisive count is that **every** `file exists` string in the whole run was one of those 48 Debug lines — the `file exists` total per node equals the Debug total per node exactly (7/9/22/10), so no EEXIST escaped to an error line. Coverage settled exact after both storms (`287 unique, duplicated=0, missing=0`), which is the outcome-level control: classifying EEXIST as satisfied did not hide a genuinely absent address. A 60s quiet window afterwards logged zero of all of it, so the noise is churn-driven, not steady-state. **Honest limitation:** only the `IPMonitor.restoreIP` arm was exercised live. The `network.BringIPup` arm never hit EEXIST in this run (`NETWORK: IP was already up when adding it` = 0 on all four) and rests on unit tests alone — though the arm that *was* proven is the one that produced 22 of run 23's 31 error lines. **`IP_FAILOVER: Some interfaces failed to bring up IPs` did still fire 7 times on node-2, but not for this cause** — all 7 trace to `IP_FAILOVER: Failed to bring IPs up remotely … DeadlineExceeded`, which is the new #57, not a netlink failure. Original finding, run 23. During the same release storm that produced zero #41 errors, the *bring-up* side logged `IP monitor restore: failed to add addr cidr=… iface=enX0 error="file exists"` (18 node-2, 4 node-3) and `NETWORK: netlink.AddrAdd failed error="file exists"` (7 node-2, 2 node-3) at **error** level, followed by `ENFORCE: Failed to bring up IP on Active node … error="unable to bring IP up as netlink failed to do so"` (8 node-2, 7 node-3) and `IP_FAILOVER: Some interfaces failed to bring up IPs`. Adding an address the node already holds is a no-op reported as a failure — exactly #41's shape with the sign flipped. Note this one propagates upward into a failed-failover report, so unlike #41 it is not purely cosmetic: a failover was reported broken because an address it wanted up was up already. **Fix:** `network.AddrAddSatisfied` classifies a failed add, and the two sites that logged these lines consult it. `EEXIST` is satisfied outright — the kernel only refuses with it when this exact address and prefix are already on this exact link, which is the goal state — and it is answered without a further netlink walk. Any *other* failure is put to a live existence check, #41's post-failure re-check in mirror image: several writers add addresses here (the enforce loop, the netlink watcher's restore, `BringUpIP`'s per-interface goroutines), so the window between a pre-check and the syscall cannot be closed, only classified. A failure on an address that is genuinely not up is still an error. Fixing `network.BringIPup` clears the `ENFORCE: Failed to bring up IP on Active node` and `IP_FAILOVER` escalation above it for this cause, since both only report what it returned. Tests: `packages/network/addr_add_test.go`, both arms mutation-tested — neutering the `EEXIST` arm fails three of the six, neutering the live check fails one |
| TC-6 | #46 `RemoveMember` self-deadlocks redistributing the leaving node's addresses | **Fixed (PR #227 follow-up), not verified live.** Found while auditing the locking the review flagged in `member_list.go`, and not itself a review finding. `MemberList` embeds a `sync.RWMutex`, which is not reentrant; `RemoveMember` takes the write lock and then calls the exported `RedistributeIPs`, which takes it again. Every removal of a node that still held floating IPs therefore hung forever **with the write lock held**, so nothing else touching the member list could make progress either — the daemon, not just the removal. Both branches of `RemoveMember` had their own copy of the call (by node ID and by hostname). Same shape as the old `RebalanceCluster` self-deadlock and the `hasQuorumLocked` one this PR already fixed, and as #32's `Load()`/`Save()`. Fix: the body is split into `redistributeIPsLocked`, which documents that it requires the lock; the exported method is the locking wrapper. The two call sites also passed `member.ActiveIPs` bare and now read it through `GetActiveIPs()`. Tests: `internal/membership/member_list_locking_test.go` — `TestRemoveMemberRedistributingDoesNotSelfDeadlock` covers both branches and times out against the old code (10s = 2 × its 5s deadline) |
| TC-6 | #47 the redistribution path reads member state without the member lock | **Fixed (PR #227 follow-up), not verified live.** Review finding. `getAvailableNodes` read `member.Status`, and `calculateIPDistribution` read `len(node.ActiveIPs)` and `node.Capacity`, holding only the member *list* lock — which does not cover those fields. `AddActiveIPs` appends to `ActiveIPs` and `UpdateConfig` refreshes `Capacity` under the member's own lock, and the health check loop writes `Status` there, so these were real races rather than staleness. `getActiveNode`, the callee of both, had the same defect. Fixed by snapshotting each member's fields under its own lock, the pattern `ConsolidationTarget` in the same file already used. Detector: `TestRedistributeIPsSnapshotsMemberStateUnderLock`, which reports a data race on every run against the old code, at exactly the three flagged lines |
| TC-6 | #48 `performPromotionAsync` writes member status bare on its rollback paths | **Fixed (PR #227 follow-up), not verified live.** Review finding. The four identical restore-from-`originalStates` loops, the two mark-unreachable blocks and the remote-promotion success all assigned `Status` (and `ActiveIPs`) directly, violating the member-locking rule this PR's own comments establish, while the health check loop and IP monitor read those fields under the member lock. Fixed with `Member.GetStatus`/`SetStatus`/`MarkUnreachable` and a `restoreMemberStates` helper; the two bare *reads* in the same function were converted too, since `-race` pairs them against the now-locked writes. **Residual, deliberately open:** the equivalent bare writes elsewhere in `internal/server` (around lines 687, 1348, 4777, 4790) are untouched — a wider pre-existing pattern the review did not scope, and `internal/membership/health_check.go`'s writes were already correctly locked |
| TC-6 | #49 `seedActiveActiveAssignments` violates `groupIPsForNode`'s lock contract | **Fixed (PR #227 follow-up), not verified live.** Review finding. The function documented itself as safe to call with or without `s.Lock()`, but calls `groupIPsForNode`, whose contract requires it because it reads `s.config.Nodes` and `s.config.Groups`. `SetMode`'s call site satisfied it (it holds the write lock); `ConfigSync`'s runs after the pointer swap has released it, so that path read the config maps unsynchronised. Fixed by stating the real contract — the caller must hold `s.Lock()` or `s.RLock()` — and taking the read lock around the `ConfigSync` call site, which is enough because the function only writes member state |
| TC-6, TC-8 | #50 `ConfigSync` writes the cluster epoch and leader outside the lock on three of its four paths | **Fixed (PR #227 follow-up), not verified live.** Review finding, extended. `157a2f9` moved the full-config branch's `clusterEpoch` write inside the critical section; the same compare-and-write remained unsynchronised in the envelope-only branch (which takes no server lock at all), in the pre-sync epoch read before the lock is acquired, and in the apply-member-states block after it is released — while the config broadcaster reads both fields under `RLock`. Fixed with `convergenceMetadata()` and `adoptConvergenceMetadata()`, which read and write the pair as one critical section. Two behaviour improvements fall out: the epoch can no longer regress (the old code compared against a snapshot taken earlier in the function, so a sync that lost the race still wrote its lower epoch), and the leader can no longer be observed against the wrong epoch. Tests: `internal/server/convergence_metadata_test.go` — `TestConvergenceMetadataIsNeverObservedMismatched` both fails the invariant and trips `-race` when the helper's lock is removed |
| TC-6 | #51 the reconciliation pass runs blocking RPCs on the 1s health-check tick | **Fixed (PR #227 follow-up), not verified live.** Review finding, and the one with real behavioural weight: `checkForActiveNodeFailure` ran inline on the tick, and below it sit a serial `MakePassive` per extra Active, a quorum vote that polls for up to 30s, a remote `BringDownIPs` per duplicate address (each carrying `Client.Send`'s own 30s deadline), and the bring-ups redistribution performs — plus `electNewActiveNode`'s retry sleeps. A 1s tick could therefore take a minute, so the node stopped answering its own health checks and peers marked it Unknown and elected around it: the same "busy node looks dead" failure that #2/#26, the batched GARP (#4) and the coordinator grace period exist to prevent, left in place on the loop that drives them. Fix: the pass runs on its own goroutine behind a single-flight `atomic.Bool`, so a tick arriving during a pass skips rather than queues, with a 3-minute backstop that releases the guard if a pass never returns. The two counters the pass reads (`reconcileCycles`, `checksWithoutChange`) moved under the health checker lock, since the tick still owns the writes. The duplicate bring-downs are also batched per node now, one RPC instead of one per address, and the consolidation loop carries a shared deadline so it cannot run away. Tests: `internal/membership/reconcile_pass_test.go` — against the inline version the dispatch test measures a 2s block on the tick and the stacking test runs 4 passes where it must run 1 |
| TC-6 | #56 the #51 fix self-deadlocks the health-check tick on its own lock | **Found and fixed on whitecrane run 24 (2026-07-30), VERIFIED LIVE.** Introduced by #51's fix. `performHealthChecks` takes the health checker's write lock at the top of its body and holds it via `defer` to the end; from inside that region it called `resetChecksWithoutChange`/`incChecksWithoutChange` (which retook the same write lock) and `startReconcilePass` (which took the read lock via the exported `IsRunning`). `sync.RWMutex` is not reentrant, so the first tick to reach any of the three wedged the health-check goroutine **while holding the write lock** — the eighth instance in this codebase of an exported-method call from inside a locked region (#32, #46, #55 are the others). Symptom on the live cluster: `ACTIVE_CHECK: Starting active node failure check` appeared **0 times in six minutes**, no node was ever promoted, nothing placed the 287-address RealTest group, and all four nodes sat Standby/Passive with the **entire group down**. Invisible to #51's own tests because they call the dispatcher directly without holding the lock. Fix: `…Locked` variants for the two counters and `startReconcilePassLocked` reading `h.ready && !h.stopped` directly, matching #55's `saveLocked` idiom. After the fix: coverage back to **287/287 with zero duplicates**, stable, and the pass runs ~1/s (289 in 5 min). Test: `TestTheTickDoesNotSelfDeadlockOnItsOwnLock` in `internal/membership/reconcile_pass_test.go`, which asserts on a timeout because a deadlock hangs the package instead of failing it — observed to fail against the deadlocking shape |
| TC-6 | #52 the demotion deadline is fixed at 10s regardless of how many addresses must be released | **Fixed (PR #227 follow-up), not verified live.** Review finding. `MakePassive` now drops **and verifies** every configured group address rather than only a node's recorded assignments (#21), so its cost scales with the group; the flat 10s on `confirmPeerReleasedIPs` was sized for the old behaviour. On this plan's own 201-address topology a healthy but loaded incumbent can exceed it, and `DeadlineExceeded` is deliberately read as "the peer is alive and may still own its IPs" — the conservative reading that keeps a wedged Active from being promoted over — so a deadline that is merely too short aborts a promotion that was safe. Fixed with `DemotionTimeoutFor(ipCount)`: a 10s base plus 100ms per address, capped at 120s so a misconfigured group cannot make the wait effectively unbounded. Applied to the consolidation invariant's demotions too, which issue the same RPC and were mis-sized the same way — affordable now that #51 moved them off the tick. Tests: `internal/membership/demotion_timeout_test.go` |
| TC-7 | #53 `PlanMoves` batch aggregation transiently exceeds a destination's capacity | **Fixed (PR #227 follow-up), not verified live.** Review finding. Capacity is enforced per simulated move, but aggregating the plan by `(src, dst, group)` collapses moves that were interleaved with others, so a destination that has to shed before it can accept beyond its capacity gets its whole incoming batch applied first. Callers apply a batch at a time, so this is observable: on a three-node topology where node2 has capacity 4, the first emitted batch put 7 addresses on it. The final state was correct either way, which is why the existing batched-equals-incremental invariant did not catch it. Fixed with `scheduleWithinCapacity`, which emits the aggregated batches greedily in plan order, trims each to the room actually available at that point and retries the remainder once the batches that free the space have been emitted. Plans with no capacities set — the common bulk-drain case, where batching turns ~150 sequential IP failovers into one call per destination — are returned untouched. Tests: `internal/ipam/plan_capacity_test.go`, whose no-transient-overflow invariant fails against the old code on the first emitted batch |
| TC-6 | #54 duplicate-IP resolution picks the survivor by record order rather than kernel state | **Partly fixed (PR #227 follow-up), not verified live.** Review finding (listed among the smaller ones). `resolveDuplicateAssignments` kept the address on whichever contender sorted first by node ID. When that is not the node actually holding it, the coordinator brings down a live address and leaves the record on a node that may not have it up, so the address is served by nobody until the next orphan sweep re-places it — one avoidable flap per duplicate, and it compounds with #16, which makes a genuine duplicate indistinguishable from a split-brain in the logs. **Only partly fixable as things stand:** no RPC exposes a peer's interface state, so the coordinator can read the kernel for the local node and nothing else. The local node's state therefore decides whenever it is one of the two contenders — which is the common case, since the coordinator running the pass is usually also a holder — and record order remains the deterministic fallback when neither is local or the interfaces cannot be read. Closing it properly needs an interface-state RPC, i.e. a proto change, which does not belong in this branch. Tests: `internal/membership/duplicate_survivor_test.go`; the two behavioural cases fail against record order and the two fallback cases are unchanged by design |
| TC-3 | #55 loading a config that needs the syslog migration deadlocks the daemon | **Fixed, NOT verified live (2026-07-30).** Found while writing the #42 tests, not on the cluster. `Config.Load()` holds the config mutex for its whole body and calls `migrateConfig`, which persisted through the **exported** `Save()` — the same non-reentrant `sync.Mutex`. Any config on disk whose four syslog fields are all empty is exactly the config the migration fires for, so loading it blocked forever: daemon startup, and every `Reload()` behind a `ConfigSync` or a `Reconfigure`. Not reached on whitecrane because its configs carry syslog settings, and invisible to the existing tests because `PULSEHA_TEST=true` short-circuits both `Load` and `Validate`. **Sixth instance of the exported-method-called-from-a-locked-region shape** in this tree (`RebalanceCluster`, `hasQuorumLocked`, #32's `Load()`/`Save()`, #46's `RemoveMember`, #42's `SetMode` delegation), so the fix is a named `saveLocked()` that documents the contract rather than another ad-hoc unlock/relock. Test: `TestLoadMigratesAnOldConfigWithoutDeadlocking`, which times out at 10s against `d4408e9` |
| TC-6 | #57 the rebalance's remote bring-up RPC has a flat 5s deadline regardless of batch size, so a large move is reported failed after it succeeded | **Fixed 2026-08-03, NOT verified live.** Both helpers now size their deadline to the batch they carry. **Bring-down is #52's shape exactly** — release and verify, per address — so `bringIPsOnNodeDown` (`internal/server/server.go:6642`) takes `membership.DemotionTimeoutFor(len(ips))`, which is what `releaseDeletedGroupIPs` already does for the same RPC. **Bring-up needed more than that, and this is the part worth stating**, because sizing it on the address work alone would have left it wrong for a subtler reason: the netlink adds are sub-millisecond, and what actually consumed run 25's 5s is the gratuitous-ARP batch the RPC ends in. `SendGARPBatch` runs `ceil(n/32)` waves of `arping -U -c 5`, each ~4s of paced packets and capped at 10s, so **a 24-address batch cannot finish inside 5s no matter how fast the addresses come up** — the deadline was below the RPC's floor, not merely below its scaling. `bringUpTimeoutFor` (line 6613) is therefore `DemotionTimeoutFor(n)` **plus** a new `network.AnnounceBatchTimeout(n)`, which lives in the package that owns `garpFanout`/`garpTimeout` rather than restating their values at the call site, capped at 120s so one move cannot block the coordinator unboundedly (24 addresses → 22.4s, 96 → 49.6s, against the 5s that failed). Tests: `internal/server/bring_up_timeout_test.go` and `TestAnnounceBatchTimeoutCoversTheWavesItWouldRun` in `packages/network/network_test.go`; all four server tests observed to fail against the flat 5s, and `TestBringUpTimeoutIncludesTheAnnouncementBatch` **also fails against the plausible-but-insufficient fix** of `DemotionTimeoutFor` alone (12.4s at 24 addresses), which is the shape this defect invites. The wave rounding was mutation-tested (`ceil` → truncating division fails on 1 and 33 addresses). **Residual:** this makes a correctly-sized deadline, not evidence — a `DeadlineExceeded` is still read by the rebalance loop as `rebalance move failed` and still breaks the loop, so the #39/#13/#21/#31 half of this row is untouched; what changes is that the deadline no longer manufactures that outcome on a move that worked. **Live check:** drive run 25's generator (`unassign` a group from a node, restart it, `assign` it back) and expect zero `IP_FAILOVER: Failed to bring IPs up remotely … DeadlineExceeded` and zero `ACTIVE_CHECK: rebalance move failed`, with coverage settling exact as it did then — and note that `IP_FAILOVER: Some interfaces failed to bring up IPs` reaching zero is only meaningful read together with the cause line above it, per the note below. Original diagnosis follows. **Found run 25 (2026-07-31).** `bringIPsOnNodeUp` (`internal/server/server.go:6232`) wraps the remote `BringUpIP` call in `context.WithTimeout(…, 5*time.Second)` with no reference to `len(ips)`. Both storms in run 25 moved 23–24 addresses per batch onto a node that was concurrently bringing up ~71, the RPC exceeded 5s, and node-2 logged `IP_FAILOVER: Failed to bring IPs up remotely … DeadlineExceeded` → `IP_FAILOVER: Some interfaces failed to bring up IPs` → `ACTIVE_CHECK: rebalance move failed count=23/24` — **7 times across the run, every one on a move whose addresses did in fact arrive** (coverage settled `287 unique, duplicated=0, missing=0` both times). Exactly #52's defect on the bring-up side: a flat deadline sized for a smaller unit of work, where #52 fixed the demotion RPC with `DemotionTimeoutFor(ipCount)`. The sibling `bringIPsOnNodeDown` (line 6252) carries the same flat 5s and was not separately measured. Same family as #39/#13/#21/#31 — the returned status is not evidence of what happened — and it matters more than log noise, because `rebalance move failed` is what the coordinator's rebalance loop reads to decide whether the move needs redoing. **This is the residual reason `IP_FAILOVER: Some interfaces failed to bring up IPs` still appears now that #45 is fixed**, so the two must not be confused: check the cause line immediately above it (`DeadlineExceeded` = #57, `unable to bring IP up as netlink failed to do so` = #45) |
| TC-6 | #59 deleting an assigned group can leave its addresses up permanently, referenced by nothing | **Fixed 2026-08-03, VERIFIED FIXED LIVE 2026-08-03 (run 27) — but see #60, which can reintroduce it, and is itself now fixed and awaiting live verification.** Live proof: six `group delete --force` runs on a 12-address group assigned to all four nodes on `enX0`, from a settled `3/3/3/3`, produced **zero strands in every run** — every node back to holding only its own `10.200.0.12N/23`, against the original defect where node-1 kept three addresses up indefinitely with no release pass ever running. Both arms were exercised: the coordinator-driven runs put node-2 on the local netlink-verified path and nodes 1/3/4 on the peer RPC path, and one run issued the delete on all four simultaneously so every node took the local path. Two runs additionally stripped the group from every node's config within 5s of the delete — the state a fixed #43 produces — and still stranded nothing. **What run 27 also found is #60:** in 2 of 6 runs a peer's IP monitor restored the addresses the release RPC had just removed, and the delete returned `Success: true` anyway; recovery came from that peer's own surplus pass against the still-configured-but-unassigned group, i.e. from this fix's own fallback rather than from its primary mechanism. That fallback is load-bearing, so **#43 must not be fixed before #60's fix is verified live** — #60 is fixed as of 2026-08-03 but unverified, so the ordering still holds until a run proves it. Fix description below. `DeleteGroup` now makes an assigned `--force` delete **two** config writes instead of one, and only completes the second if the addresses are accounted for. Write 1 drops every assignment and commits it, which leaves the group *configured but unassigned* — the one state whose release pass is verified working live (#58), so a node that misses what follows still converges on its own instead of stranding. Then the addresses are released on every node that holds them, concurrently and **outside `s.Lock()`** (a fan-out under the lock is what makes a node look dead — #4/#7/#8). Write 2 deletes the group, and runs **only** if every release was confirmed; otherwise the response is `Success: false` naming the nodes, the group stays configured and unassigned, and a retry finishes the job. The ordering is deliberately *not* the note's first suggestion of "release before the group leaves the config": releasing while the group is still **assigned** races the enforce loop, because an active-passive Active expects the whole configured group and its next tick re-adds everything just released. Per-node plan comes from `expectedIfaceIPs`, so active-active sends each node only its assigned share rather than asking a node to bring down a group it holds none of (#34's 201 error lines for a no-op); an address a still-configured group provides on the same interface is excluded, which the CLI cannot produce but the appliance's own config writes can (#3). What counts as confirmation follows #21: for a peer, only transport failure is fatal — its per-address netlink failures are invisible (no RPC exposes peer interface state, the wall #54 hit) and are the benign `cannot assign requested address` case anyway; for the local node the release goes through the `BringDownIP` handler (so monitor expectations and the assignment list stay honest — #58) and is then **checked against the kernel**, with `RemoveActiveIPs` called mode-independently since nothing can recompute a deleted group's addresses downward. Release deadline is `DemotionTimeoutFor(largest batch)` rather than a flat one, which is #57's defect on the release side. Tests: `internal/server/group_delete_test.go` — `TestForceDeleteReleasesTheGroupsAddressesOnEveryNode`, `TestForceDeleteDropsTheAssignmentBeforeReleasing` and `TestForceDeleteKeepsTheGroupWhenAPeerReleaseCannotBeConfirmed` all fail against the unfixed handler; `TestForceDeleteSpareAnAddressAnotherGroupStillProvides` was mutation-tested. **Residual:** the local kernel check is netlink, so it is Linux-only and covered by the live path rather than these tests, and a node unreachable for both writes releases via its own pass rather than this one. Original diagnosis below. `pulsectl group delete VerifyTest --force` on a 12-address group assigned to all four: nodes 2, 3 and 4 released their 3 each, node-1 kept `10.200.0.155/23`, `.159`, `.163` up indefinitely — still there after 2 minutes with no release pass ever running for them, and no group left in config to reference them. Cause: `--force` removes every assignment and deletes the group in **one** config write, so the group is never "configured but unassigned" for long enough for a node's enforce tick to compute surplus; and once it is gone from `config.Groups`, `surplusFloatingIPs` cannot see the addresses at all, because it scans only configured groups (deliberately — anything else would be the node's own addresses). Whichever nodes' tick happens to fall inside the propagation window release; the rest strand. Distinct from #58, and #58's fix is what makes it *visible*: the node now honestly reports `Active` with 3 held, where before every node reported a stale full list and the strand was indistinguishable from the lie. Recovery used here, which doubles as the reproduction in reverse: create a group over exactly the stranded addresses, assign it to the holding node, then `unassign` — that makes them surplus against a *configured* group and the release pass takes them down. Suspected fix: release before the group leaves the config, or refuse `--force` until assignments have been dropped and propagated | 
| TC-6 | #60 a deleted group's release RPC races the peer's IP monitor, which restores the addresses it just removed — and the delete reports success anyway | **Fixed 2026-08-03, VERIFIED FIXED LIVE 2026-08-03 (run 28).** Was the gate on fixing #43; **that gate is now cleared.** **Fix: an address the node has been told to release is protected from the node's own restore paths for 60s.** `RemoveExpectedIPs` — which every deliberate release already calls, the bring-down RPC included — now also records the address as released, and both paths that put an address back consult that record: the netlink watcher's restore and the enforce pass's bring-up, through one shared `restorableIPs` decision rather than two. Dropping the expectation could never carry this on its own, which is why the original fix attempt looked sufficient and was not: both restore paths *re-derive* their expectations from the config, in active-active from the node's own assignment list and in active-passive from the whole configured group, and the node being commanded to release is by construction the node whose config has not yet been told the address stopped being its share. It removed the expectation, re-derived the same expectation moments later, saw the address missing and restored it. That is the note's option 2 below, done so that the refresh cannot undo it — **option 1 was rejected** because with #43 unfixed a non-coordinator's write 1 may not propagate at all, so a peer that refused an out-of-date release would make every such delete return `Success: false`, and **option 3 needs an RPC exposing peer interface state that does not exist** (#54's wall). The protection is deliberately a backstop with three ways out, not a state: it lapses after 60s (observed restore window was ≤22s and the enforce period is 30s); `AddExpectedIPs` clears it, which is what `BringUpIP` calls per address immediately before putting the address back, so a re-assignment wins at once; and it is moot as soon as the config catches up, since an expectation set derived from a config that no longer gives the node the address restores nothing. The config-derived setters — `UpdateExpectedIPs` (every config sync's refresh) and `UpdateExpectedIPsAll` (the enforce loop's own write-back) — deliberately do **not** clear it: on the node mid-release the config they read is exactly what lags, so clearing there re-arms the defect. Nothing gates the *release* pass, only restores, so a released address is still surplus and still comes down. Tests: `internal/membership/ip_monitor_release_protection_test.go` — `TestAReleasedAddressIsNotRestoredWhileTheConfigStillClaimsIt` (drives the resurrection through `deriveExpectedIPs` and asserts only the released address stays down), `TestTheWatcherLeavesAReleasedAddressDown`, `TestReleaseProtectionIgnoresTheMask`, `TestReleaseProtectionExpires`, `TestOnlyAnExplicitReassignmentClearsTheProtection`; all five observed to fail in a throwaway worktree with the single arming call removed. **Residual:** peer confirmation is still transport-only, so this removes the cause of the false success rather than adding evidence against it — a peer whose netlink release genuinely failed for a reason other than "already gone" is still waved through, per #21's rule; and the two consumers are in `ip_monitor_linux.go`, so the wiring itself is covered by the live path rather than by these tests, as with #58. **Run 28 result (2026-08-03), binary `188c2d9b` (`671ec04`) verified on the running process of all four: 14 `group delete --force` cycles, zero strands and zero restores in every one.** `expected IP removed from Active node; restoring` = **0** on all four nodes in all 14 runs, with the group's addresses gone cluster-wide at +12s and still gone at +42s each time. The gate is proved to have *fired* rather than the race merely not landing: the suppression lines appeared in **4** runs — run 1 node-3 (1 watcher + 2 enforce), run 11 node-1 (6 + 6), run 13 node-1 (7 + 9) and node-3 (1 + 1), plus the re-add check below (node-3 ×3, node-4 ×2) — with the pre-fix sequence intact right up to the point it diverges: `RPC BringDownIP … for 3 IP(s)` → `Removed IPs from interface …` → `IP monitor: expected IP was released on request; not restoring ip=10.200.0.161/23` where run 27 had `… restoring`. Both consumers are covered, which matters because the wiring in `ip_monitor_linux.go` is exactly what the unit tests cannot reach. **Widening the group is what makes this reproducible on demand:** at 12 addresses (`3/3/3/3`) the race arose in 1 of 9 runs, at 36 (`9/9/9/9`) in 2 of 3 — a larger write 1 takes the peer longer to apply, and that lag *is* the window. **The 60s backstop was also shown not to block a legitimate re-add:** with the group re-created ~26s after a delete, node-3 brought `10.200.0.158` up at 14:29:03 having been told to release it at 14:28:38 — 25s in, well inside the window — so `AddExpectedIPs` clears the record as designed. Coverage returning over ~45s rather than at once is ordinary placement latency; judge this on the per-address bring-up timestamps, because the cluster-wide total cannot distinguish a cleared record from a lapsed one. **#43 reconfirmed in passing** (run 10: after a coordinator-only delete, node-2 had dropped the group while nodes 1/3/4 still listed it). **Original diagnosis, run 27.** Observed in **2 of 6** `group delete --force` runs, on whichever peer's monitor tick fell inside the window. Sequence, from node-3 at 13:11:03 (node-4 at 12:55:46 is the same shape): `RPC BringDownIP on iface enX0 for 3 IP(s)` → `Removed IPs from interface … remaining=[10.200.0.161/23 10.200.0.165/23]` → **`IP monitor: expected IP removed from Active node; restoring ip=10.200.0.157/23`** → the other two removed → `ENFORCE: releasing floating IPs this node is no longer assigned … count=1`. The peer had not yet applied write 1, so its monitor still expected its old assigned share and treated the commanded release as an address that had gone missing. The restore window varied from **sub-second** (node-3) to **22 seconds** (node-4, restoring all three of its addresses and releasing them only at 12:56:08). `DeleteGroup` still returned `Success: true` and proceeded to write 2, because peer confirmation is transport-only — #21's rule applied to a case it does not cover: the RPC genuinely succeeded, and the peer then undid it. **Why this is the thing to fix before #43:** what saved every run was the peer's own surplus pass against a group that was still *configured* but unassigned there — write 2 never reached those nodes at all, which is #43. Once deletes propagate correctly, a peer that restores its share and then loses the group from its config before its next surplus pass has those addresses outside every set any pass can compute, which is precisely **#59 again, permanently**. Both ingredients were observed separately in run 27; the combination was not, only because #43 kept write 2 local. Note the two are *not* independent — this is a case where fixing one defect re-arms another. **Fix shape:** the release must not land on a peer before that peer has applied the assignment drop. Options, roughly in order of preference: carry the required `config_version` on the release RPC and have the peer apply the pending config (or refuse) before releasing; refresh the peer's monitor expectations as part of the release rather than leaving the two paths to race; or make write 2 wait on each peer reporting the addresses *observably* down instead of on transport success — the same shape as `8ffc1c1`'s release verification for promotion, which is the existing precedent for "a boolean is not evidence" (#21). Reproduce with `scratchpad/freq.sh`'s shape: settle a 12-address group at `3/3/3/3` across four nodes, `group delete --force` **on the coordinator only** so the other three take the peer path, then grep those three for `expected IP removed from Active node; restoring` — expect a hit on roughly one run in three |
| TC-6 | #58 the enforce loop's release pass never updates the node's own assignment list, so a released address is reported as held forever | **Fixed 2026-08-03, VERIFIED FIXED LIVE 2026-08-03.** Live proof: every `ENFORCE: releasing floating IPs this node is no longer assigned … count=N` is paired with `ENFORCE: dropped released floating IPs from this node's assignments count=N` at the same count, on all four nodes (4/1/4/4 pairs), and the node's reported list empties to `Standby` with `ip -4 -o addr show` agreeing. Note the release pass is **not** what `group delete --force` exercises (see #59) — reproducing it needs `unassign` while the group stays configured. Found on whitecrane three days after run 25 ended: `pulsectl status` reported all four nodes `Active` holding 288 addresses of the deleted `RealTest` group while `ip a` showed only each node's own address on all four. The release had worked correctly — one `ENFORCE: releasing floating IPs this node is no longer assigned … count=72` per node at 08:12:11–08:12:24 on 2026-07-31, kernel clean ever since — but `Active IPs` is `member.ActiveIPs` (`internal/cli/status.go:159` ← `internal/server/server.go:1253`), pure in-memory bookkeeping maintained incrementally by `BringUpIP` (append) and the `BringDownIP` RPC handler (subtract). The enforce loop's release pass calls `network.BringIPdown` directly (`internal/membership/ip_monitor_linux.go:440`), bypassing that handler, so in practice the list was append-only. Neither `DeleteGroup` nor `UnassignGroupFromNode` clears it either. It cannot self-heal: `surplusFloatingIPs` only scans *configured* groups, so a deleted group's addresses are outside every set it computes (deliberate, and correct — those would otherwise be the node's own addresses); `deriveExpectedIPs` correctly yields nothing to re-add but never recomputes the list downward; and a peer's copy is only overwritten by a **non-nil** self-report (`internal/server/server.go:4964`), so an empty list reads as "no information" and the stale copy survives every sync. **Not merely cosmetic:** `deriveMemberStatus` reports `Standby` only on an empty list, so all four reported `Active` while serving nothing — #40's operator-visible lie in a new path — and `calculateIPDistribution` (`internal/membership/member_list.go:241`) plus `leastLoadedNodeForGroup` (`internal/server/server.go:3200`) both read `len(ActiveIPs)` as the node's load. Here all four were equally wrong (72 each) with `capacity` unset, so relative balance would have survived, but every absolute-count decision was reading fiction. **Fix:** the release pass now drops what it released from the local member via a new `Member.RemoveActiveIPs` (bookkeeping only — the caller has already brought the addresses down). A release that *failed* on an address the node still holds deliberately stays on the list, so it is retried rather than stranded as #40 did. Tests: `internal/membership/ip_monitor_release_test.go` — `TestReleasedAddressesLeaveTheAssignmentList` (classification: released and vanished drop, failed stays), `TestRemoveActiveIPsDropsOnlyTheGivenAddresses`, `TestRemoveActiveIPsCanEmptyTheList`. **Residual:** the one-line wiring in `ip_monitor_linux.go` is Linux-only and so is covered by the live path, not by these unit tests |

---

## Result 2026-08-03 (run 26) — #58 found, fixed and VERIFIED FIXED LIVE; #59 opened

The cluster was inspected three days after run 25 and the state disagreed with
run 25's own leave-behind note: `pulsectl status` on all four nodes reported every node `Active`
with 72 addresses each (288 total) from `RealTest`, while `ip -4 -o addr show enX0` returned only
`10.200.0.12N/23` on all four, and `RealTest` was absent from `floating_ip_groups` in
`/etc/pulseha/config.json` on all four. `ip a` was right.

**Run 25's leave-behind is superseded.** It records `RealTest` = 287 assigned to all four and
settled `72/72/72/71`. The group was in fact unassigned from all four and deleted at 08:12:11 on
2026-07-31, minutes after that note's window — `Received DeleteGroup request for group: RealTest`
→ `Successfully deleted group RealTest`, config written 08:13:33. The current groups are
`Management` (empty) and `test` (empty, on `enX1`). Anything planning to reuse `RealTest` must
recreate it, DAD-verifying the range first as always.

Diagnosis is in the #58 row. Two things worth keeping separate from it:

- **The release itself was correct, and that is what made the defect hard to see.** Exactly one
  `ENFORCE: releasing floating IPs this node is no longer assigned … count=72` per node, no
  `failed to release` lines, and the addresses have stayed down for three days across no restarts
  (daemons up since 07:44–07:57 on 2026-07-31, all still on
  `c3b56a2a907cda6f9aa11a13f717356c`). Every mechanism except the bookkeeping did its job, so the
  only visible symptom was a status output that contradicted the kernel.
- **The stale state is purely in-memory.** Nothing persists `ActiveIPs`; a rolling restart clears
  it. `mode: active-active` *is* persisted on all four (under `pulseha.mode`), so the mode
  survives one. `/run/lbBootFlag` is present, so `lbClearRestart` runs on start — harmless here,
  there is nothing left for it to wipe.

Unit suite green with `-race` across `internal/`, `packages/` and `cmd/` (12 packages), and the
tree cross-compiles for `linux/amd64`.

### Live verification (run 26)

Deployed `42085f65900bc0f671058e399ce0cfce` to all four and rolled the restarts one at a time,
verified via `/proc/MainPID/exe`. Baseline after the restarts was honest and is itself half the
evidence: all four `Standby`, reported 0, actual 0 — where before the restart the same command
said `Active` with 72 each.

Group `VerifyTest`, 12 addresses (`10.200.0.152-163`), DAD-verified free first with a **positive
control that fired** (`10.200.0.1`, `.122`, `.123` all `rc=1` in use; all 12 candidates `rc=0`).
Assigned to all four on `enX0`, settled to `3/3/3/3` with reported matching actual on every node.

**The fix fires, paired, on all four:** every `ENFORCE: releasing floating IPs this node is no
longer assigned … count=N` has a matching `ENFORCE: dropped released floating IPs from this
node's assignments count=N` at the same count — 4 pairs on node-1, 1 on node-2, 4 on node-3, 4 on
node-4 — and the released node's reported list empties to `Standby` with `ip a` agreeing.

Two traps this run walked into, both worth inheriting:

- **`group delete --force` does not exercise the release pass.** It was the obvious test and it
  proves nothing about #58: nodes 2/3/4 came clean via the coordinator's redistribution, which
  goes through the `BringDownIP` RPC and always maintained the list correctly. The pass only fires
  where a group stays **configured** while ceasing to be assigned — i.e. `unassign`, which is what
  the original incident used four times over. This is also how #59 was found.
- **The node clock is behind the workstation's**, so `journalctl --since "12:10"` was a window in
  the *future* and returned zero of everything — reading exactly like "the code never ran", and it
  briefly did. `date +"%F %T"` on the node first, every time; the existing warning in this
  document about the displayed clock is not the only skew on these hosts.

**Leave-behind.** Groups `Management` (empty) and `test` (empty, `enX1`) on all four, consistent —
verified after cleanup, because the group *deletes* did not propagate (**#43 again**: creates and
`add-ip` propagated to all four, deletes committed locally only, and a re-broadcast from a node
that still held the group resurrected it on nodes that had just deleted it). Clearing it needed
the delete issued on all four **simultaneously**. All four `Standby`, zero floating IPs, running
`42085f65900bc0f671058e399ce0cfce`, `/run/lbBootFlag` **restored on all four**.

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

## Result 2026-07-28 (run 19) — TC-3 PASSES, defect #5 verified fixed live; #37 and #38 opened

Binary `2189d015cd11` (build `2af3b80`), deployed to all four and md5-verified, rolling restart one
node at a time, which preserved the `51/50/50/50` baseline exactly. The cluster stayed in
**active-active** for the whole run — the harder variant, since every mutation also drives
placement. The `RealTest` group was cleared 201 → 1, then 200 addresses added back-to-back from
node-1.

### TC-3 — **PASS**

All four configs were identical at every sample during a 43-minute mutation run (97/97/97/97;
139/139/139/140 as the single transient; 181×4) and identical at the end. The historic signature —
200/189/192/193 diverging and *staying* diverged — did not occur. The removal direction converged
too: 200 serial `remove-ip` calls took all four from 201 to 1 with zero divergence. Final settled
state `51/50/50/50`, `placements=201 unique=201 duplicated=0`.

### Defect #38 — uniform loss, which TC-3 cannot see

Nine of the 200 adds (`10.200.0.156/181/182/192/197/203/220/222/224`) ended absent from **all four**
configs, the identical set on each. node-1 logged `Successfully added IP … to group RealTest` for
each, and only 2 of the 200 CLI calls returned non-zero. Because every node agreed, TC-3's "all four
configs match" criterion scored the run a pass — the case is uniform loss, not divergence, and needs
a separate check that the configured set matches what was asked for.

Correlated evidence on the wire: 17 × `CONFIG_BROADCAST: peers did not accept the config after all
retries` on node-1, 16 of them naming node-2, plus one push from node-2 at `generation=0` at
16:04:57 — inside the window where `.181` and `.182` were added and lost. Re-adding the nine one at
a time stuck permanently, so it bites only under sustained back-to-back mutation.

Root cause, found by reading the `2af3b80` guard rather than by re-running the cluster: the
per-sender generation could not express "this peer is behind". See the #38 row in §5 for the
mechanism and the fix. Two detectors, each verified to fail with the mechanism removed:
`TestABehindPeerCannotEraseANewerConfigFromAnotherSender` (reproduces the 200 → 189 erasure under
the old comparison) and `TestApplyingAPeerConfigAdoptsItsVersion` (pins the `generation=0` half).

**Still to verify live**, from run 19's settled baseline: a ~40-add stress batch (9/200 ≈ 4.5%, so
expect ~2 losses pre-fix) with `logging_level=debug` on both node-1 and node-2, checking that the
configured count on all four nodes equals the number of adds issued. node-2 was left on
`logging_level=debug` for this.

### Logging trap that cost time and produced one wrong conclusion

Every `CONFIG_BROADCAST` / `CONFIG_RECONCILE` line except the "did not accept" Warn is `Debug`, and
the shipped default is `logging_level: info`, so they are invisible. "The periodic reconcile never
fires" was concluded from 25 `reconcileConfigAcrossPeers` calls with zero log lines on all four
nodes — **that was wrong**. With `logging_level=debug` on node-2 it fires exactly as designed, once
a minute on the coordinator. Run a control first: a Debug line known to be frequent, e.g. `heartbeat
convergence nudge` (every 3 checks). If that is absent, Debug is off and no absence proves anything.
Log level is applied only at startup (`cmd/pulseha/main.go:77`) and is deliberately preserved across
ConfigSync (`server.go`), so set it per node in `config.json` and restart that node.

### Coordinator identity on whitecrane

`clusterCoordinator` is the lowest UUID among healthy nodes, and the UUIDs are `049…`=node-2,
`125…`=node-4, `16f…`=node-3, `b83…`=node-1 — so **node-2 is the coordinator**, and therefore the
only node that re-broadcasts. Worth knowing before attributing any coordinator-gated behaviour.

### Harness

Clearing a group leaves the addresses up on the interfaces: 158 orphans survived 201 → 1 and did not
drain in 30s+ (§4 teardown's documented "orphans survive group deletion"). Strip them with
`ip addr del` before re-adding or the next run's counts are meaningless.

## Result 2026-07-28 (run 20) — defect #38 VERIFIED FIXED LIVE; #39 opened

Binary `6884eb0f2149` (code `e2c7143`), deployed and md5-verified on all four, rolling restart
(one node at a time) from run 19's settled `51/50/50/50`. Cluster stayed in **active-active**
throughout. Debug enabled on all four first, with the `heartbeat convergence nudge` control
checked (13 lines in 60s on node-2) so that no absence-of-logs conclusion could be drawn from a
Debug-off node — the trap that produced a wrong conclusion in run 19.

**Method — deliberately make the coordinator fall behind, two ways.** TC-3's own criterion
("all four agree") structurally cannot detect #38, so the criterion here is *the configured
count equals the number of adds issued*. node-2 is the coordinator (lowest UUID `049…`) and is
the node that re-broadcasts, so it is the one that has to be put behind.

- **Variant A — SIGSTOP/SIGCONT (20 adds, `10.200.1.97-119`).** Paused after add 5, resumed
  after add 12, so it missed 7 adds while **keeping** its in-memory `configVersion`.
- **Variant B — stop/start (12 adds, `10.200.1.132-143`).** Stopped after add 3, started after
  add 9, so it missed 6 adds and booted at `configVersion 0` reading 236 against the cluster's
  242. This is the sharper test: `buildFullConfigPayload` omits the metadata at 0 and
  `configIsNewer` returns true for an unversioned payload (`server.go:6027`, `6287`), so an
  unversioned push from a behind coordinator would be applied unconditionally — a path
  `e2c7143` deliberately does **not** guard.
- A middle batch of 12 (`10.200.1.120-131`) ran with node-2 up throughout.

**Result: PASS. 44 adds issued, 44 landed, 0 losses.** 201 → 245, all four configs byte-identical
at every checkpoint, all 44 addresses present on all four, settled `61/61/61/62` with
`placements=245 unique=245 duplicated=0`. Pre-fix rate was 9/200 ≈ 4.5%, so ~2 losses were
expected without the fix.

**The guard was observed working on the wire — the first live confirmation.** Both halves:
- Sender side, new in `e2c7143`: `CONFIG_BROADCAST: peer holds a newer config; this node's
  change will not propagate and will be reverted by the next sync peer=… version=44`, once per
  peer, on each of node-2's two post-restart reconcile firings.
- Receiver side: `CONFIG_SYNC: ignoring superseded config sender=049-b22-093-2d3 version=20
  held=20` on node-3.

**Variant B is the one that reproduced #38's actual mechanism; variant A did not.** In A the
rejection was `version=20 held=20` — *equal*, so it was the node-ID tiebreak, not staleness:
node-2 had already adopted the current version and content before its reconcile fired, so it
never held stale content to push. In B the rejection was a genuine `version < held` from a
coordinator that really was behind — exactly the broadcast that erased adds in run 19.

**The unversioned (version-0) hole did not materialise, and remains a narrow open race.** In
both restart attempts `adoptConfigVersion` raised node-2 from 0 to the cluster's version off an
inbound full ConfigSync *before* its once-a-minute reconcile timer fired, so it never broadcast
unversioned. The window is real but small: it needs a reconcile to fire before the first inbound
full config lands. Not demonstrated as a defect; recorded so it is not mistaken for tested.

### New defect

- **#39 — an `add-ip` that returns rc=1 has still been applied. NEW, OPEN.** Two of the 20
  variant-A adds (`10.200.1.105`, `.112`) exited **1** with
  `Failed to add IP to group: rpc error: code = DeadlineExceeded desc = stream terminated by
  RST_STREAM with error code: CANCEL`, and both addresses were nonetheless present in the
  configured group on **all four** nodes afterwards (201 + 20 = 221, not 219). The RPC deadline
  expires on the caller while the mutation has already been applied and broadcast server-side.
  Same family as #13/#21/#31 — the status returned to the operator is not evidence of what
  happened — but inverted: here a *failure* is reported for work that succeeded. Consequence for
  testing: an add that returns non-zero cannot be excluded from the expected count, which is the
  opposite of what the run-20 runbook assumed. The deadline is reachable because of #37: each
  add fans out serially with inline per-IP GARP, so an add takes ~13s normally and ~28s with one
  peer unreachable.
  **ROOT-CAUSED AND FIXED 2026-07-28 (statically, from the code — no live repro was needed to
  find it; see the #39 row in §5 for the fix).** Three facts met: `Client.Send` puts a 30s
  deadline on every CLI→daemon call and `internal/cli/group.go` sets none of its own;
  `AddIPToGroup` held `s.Lock()` and ran the serial per-peer `BringUpIP` fan-out **before** it
  touched the config, ~2s inside that deadline at #37's 28s worst case; and the handler never
  checked `ctx`, so when the deadline fired it carried on to the append, `Save()` and
  `markConfigDirty()`. The mutation was durable and broadcast — only the report was lost. **#37 is
  therefore not merely "why #39 is reachable", it is the same defect's other face**, and the fix
  (commit first, then fan out concurrently and asynchronously) addresses both.
  **Live verification owed:** from a settled baseline, stop one node so the fan-out has a dead
  peer to wait on — the run-20 condition that produced the two rc=1 adds — then issue ~20
  `add-ip` calls timing each one. Expect every add to exit **0** and to take well under a second
  of daemon time (the old figure was ~28s with a peer down), the configured count on the three
  live nodes to equal the number issued, and the stopped node to pick the additions up from
  config on restart. The negative control matters as much as the count: a run in which no add
  exceeds a couple of seconds proves the deadline is no longer reachable, so a *zero* rc=1 count
  on a fast run is not by itself evidence.

### Other observations

- **#37 reconfirmed and refined.** ~12-13s per add with all four up, ~28s with node-2
  unreachable (the fan-out waits on the dead peer), and as low as 4-9s once the cluster was
  mid-churn. The 28s figure is what makes #39's deadline reachable.
- **A single node restart churns placement hard.** Immediately after variant B's restart:
  `n1=67 n2=60 n3=66 n4=108`, `placements=301 unique=220 duplicated=73` — 24 addresses down
  cluster-wide and node-4 holding 108 against a ~61 target. It converged fully to
  `duplicated=0` within ~4 min without intervention. Same family as #23, but from a *rolling*
  restart of one node rather than a cold start of four.
- The rolling restart onto the new binary preserved the `51/50/50/50` baseline exactly, as in
  run 19 — that procedure is reliable and should stay the default.

### Harness notes

- **`journalctl --since "-20min"` and `--since "18:05:00"` did not return the same window** on
  these hosts: the relative form gave 9 `CONFIG_RECONCILE` hits where the absolute form gave 3,
  and it initially reported **zero** `peer holds a newer config` lines that the absolute form
  found. Use absolute timestamps when a count is the evidence.
- `grep -c pattern file || echo 0` prints **two** zeroes when nothing matches (grep prints `0`
  *and* exits 1), so any `[ "$(count)" -lt N ]` downstream dies with `integer expected`. This
  silently collapsed the first variant-B attempt into a stop+start 1s apart, before the batch had
  even begun. Drop the fallback — `grep -c` already prints 0.
- The configured group stores addresses **with the prefix** (`10.200.0.152/23`) while
  `ip -o addr` yields bare addresses; comparing the two sets without stripping `/23` scores
  every node as holding nothing.
- Address probing: `10.200.1.101`, `.102`, `.103` are genuinely occupied by something on the
  segment — `arping -D` with a positive control (gateway + two cluster IPs must come back LIVE)
  correctly excluded them. `10.200.1.97-100` and `.104-.143` were free.

### Leave-behind

`RealTest` = **245** addresses (`10.200.0.152-255`, `10.200.1.0-96`, `.97-100`, `.104-143`),
mode **active-active**, settled `61/61/61/62`. `logging_level` = **debug on node-1 and node-2**
(the runbook's prescription), restored to **info on node-3 and node-4** — note the level is
applied only at startup, so nodes 3 and 4 keep logging at debug until their next restart.
`/run/lbBootFlag` is **absent on all four**, unchanged by this run, as run 19 left it: restoring
it would let `lbClearRestart` wipe the 245 hand-added test addresses on the next daemon start
(defect #3).

## Result 2026-07-28 (run 21) — Standby VERIFIED FIXED LIVE; #40 opened

Binary `4aca395059229e0b8837454155a900b0` (code `5caf136`: `ac1e103` plus the `origin/dev` merge),
deployed to all four and md5-verified. Rolling restart one node at a time, coordinator (node-2)
last, from run 20's `61/61/61/62`.

**The rolling restart was seamless this time** — each node kept its exact share across its own
restart and total coverage never left 245. Run 20's `duplicated=73` churn did not recur, so that
is not an inevitable consequence of restarting; worth knowing before attributing churn to a
deploy.

### Standby: PASSES

`Status: Standby` observed on node-3 both **locally** and **from node-1's peer view**, in
active-active, with `Cluster Status: online` — so the `calculateClusterHealth` half of `ac1e103`
holds too: a node serving nothing does not drag the cluster to degraded. The `Active IPs:` line is
absent for a Standby node, and `RealTest`'s `Assigned to:` correctly listed only nodes 1, 2 and 4.

**Getting a node to that state took three attempts, and the two failures are the instructive
part** — both looked like the feature not working and were in fact correct output from a stale
input:

1. **Peer view from an un-upgraded daemon.** During node-3's own restart it briefly held nothing
   and node-1 reported it `Active`. `pulsectl status` is answered by the *local* daemon, and
   node-1 was still on the old binary. A status assertion is only meaningful once the **queried**
   node is upgraded — not merely the node in question.
2. **Hand-stripping addresses does not update the record.** After `ip addr del` of node-3's 61
   addresses it still reported `Active` with `activeIPs=61`. The derivation reads the *assignment
   record*, not interface state, and nothing reconciles the record against reality — so `Active`
   was the correct answer for the input it had. Restarting node-3 cleared the record and `Standby`
   appeared immediately.

So the deterministic recipe is: unassign the group, strip the addresses, **restart the node**.

### #40 — unassigning a group from a node strands its addresses permanently. NEW, OPEN.

`group unassign --group RealTest --node-id <node-3> --interface enX0` returned rc=0 and the
config propagated correctly to all four nodes (node-3's `group_assignments` became
`{'enX0': ['Management']}` everywhere). Node-3 nonetheless **kept serving all 61 of its RealTest
addresses**, and after they were stripped by hand the cluster sat at **184/245 — 61 addresses
down cluster-wide — indefinitely** (still 61 short after 7 minutes and 15 reclaim attempts).
Recovered fully within 20s by reassigning the group.

Two independent halves, either sufficient:

1. **The release pass cannot see them.** `releaseUnassignedIPs`
   (`internal/membership/ip_monitor_linux.go:376`) iterates `localNodeCfg.IPGroups` and draws its
   surplus set only from `config.Groups[groupName]` for those **currently assigned** groups
   (lines 382, 389-390). Unassigning removes `RealTest` from that loop entirely, so the 61 held
   addresses fall outside every set the pass can compute. node-3's journal shows it knew perfectly
   well it expected nothing — `ENFORCE: Current expectations expectations=map[]`, `status=Active` —
   and released nothing on every tick. The Active branch only adds; the release counterpart
   (runs 8-14, fault 6) is scoped to assigned groups.
2. **The orphan reclaim targets the node it just unassigned, and never retries elsewhere.** Once
   the addresses are on no node, the coordinator does detect them and does the right thing up to
   the last step: `Initiating vote for redistribution of 61 IPs` → `Concluded voting session:
   passed=true, quorum=true, yes=3, no=0, total=3` → `ACTIVE_CHECK: redistributing 61 orphaned
   floating IP(s)` → `ERRO Failed to assign IPs to node hostname=MC-LB-node-3 error="group
   RealTest not assigned to any interface on node MC-LB-node-3"`. Placement does not filter
   candidates by group assignment, the assign is correctly refused, and **nothing retries onto an
   eligible node** — 15 cycles, 33 passed votes, ~30s apart, zero addresses placed.

Half 2 is the more serious: a permanent outage behind a *passing* quorum vote, on a path whose
whole purpose is to recover orphans. It is #13's shape (a failed assign is never retried) with an
additional planner bug (an ineligible target is chosen in the first place). Half 1 is the quieter
one but is the operator-visible lie: `unassign` reports success while the node it removed keeps
serving the group's traffic, with no log line anywhere saying so.

Fix shape: filter placement candidates to nodes the group is actually assigned to (half 2), retry
the assign against the remaining candidates rather than dropping it (half 2, #13), and give the
release pass a whole-group view that survives unassignment — the same tension #30 recorded, where
the release direction was deliberately kept whole-group *because* a node may hold addresses it was
never assigned. Unassignment is exactly that case and the current scoping misses it.

### Leave-behind

`RealTest` = **245**, mode **active-active**, settled `61/62/61/61`,
`placements=245 unique=245 duplicated=0` verified. All four on
`4aca395059229e0b8837454155a900b0`. `logging_level` = debug on node-1 and node-2, info on node-3
and node-4 (and now actually running at info, since all four were restarted this run).
`/run/lbBootFlag` **absent on all four**, unchanged.

## Result 2026-07-28 (run 22) — #40 VERIFIED FIXED LIVE, both halves and the rebalance direction

Binary `70708a5fc74ccf59088ed72390dab67f` / `pulsectl 3fceaadc4dde2a0a5edef213f473042b`
(code `b6c2431`), deployed to all four and md5-verified on disk. Mode active-active, `RealTest`
= 245 addresses, starting from run 21's leave-behind `61/62/61/61`.

### The restart that did not happen — check the *running* binary, not the deployed file

The first rolling restart silently failed on three of four nodes: the restarts were issued from a
shell function whose `ssh` calls had their stdin consumed by the enclosing loop, with stderr sent
to `/dev/null`. `systemctl restart` never ran, and every subsequent measurement was the old binary
still running untouched. This looked exactly like the fix not working: `unassign` returned rc=0,
the config propagated to node-3 (`group_assignments` became `{'enX0': ['Management']}`),
expectations went correctly empty (`ENFORCE: Current expectations expectations=map[]` from
20:49:35), node-3 stayed `Active` holding all 61 addresses, and nothing was released — for four
and a half minutes.

The on-disk md5 was correct on all four, so the usual check passed. What caught it was
`md5sum /proc/$(pgrep -f /usr/local/sbin/pulseha)/exe` together with the process start time:
node-3's daemon had been up since 19:31:34, before the deploy, running `4aca3950…`. **Verify the
running process, not the installed file** — this is run 21's lesson ("a status assertion is only
meaningful once the *queried* node is upgraded") in a new form, and it cost the same wasted
diagnosis. Incidentally it re-confirmed #40 on the old binary independently.

### #40: PASSES — all three parts

Restarting node-3 on the new binary at 19:53:57 UTC, with `RealTest` still unassigned from it:

1. **The release pass sees the unassigned group.** Two seconds later, node-3:
   `WARN ENFORCE: releasing floating IPs this node is no longer assigned iface=enX0 count=61`.
   All 61 addresses of the group it no longer holds an assignment for, released by the node itself
   — the half that used to compute an empty surplus set on every tick while `unassign` reported
   success.
2. **The reclaim places them on eligible nodes.** Coverage went to `81/82/0/82` within ~15s:
   `unique=245/245 missing=0 duplicated=0`. Across eight minutes and every reclaim cycle there
   were **zero** `group RealTest not assigned to any interface on node MC-LB-node-3` refusals and
   zero `Failed to assign IPs to node` on any of the four nodes. Run 21's 33 passed votes with
   zero placements are gone: the vote now concludes and the addresses land.
3. **Rebalance declines instead of stalling.** With node-3 correctly ineligible and holding
   nothing, the cluster is deliberately imbalanced — and `ACTIVE_CHECK: rebalancing` fired **zero**
   times, with zero `rebalance move failed`. The planner rules the ineligible destination out
   rather than choosing it, failing `OrchestrateIPFailover` and breaking out of the loop. Without
   this the reclaim fix would only have moved the stall: the empty fourth node is exactly the
   least-loaded destination the old planner would have picked. `81/82/0/82` held steady for 100s+
   with no oscillation.

node-3 reported `Status: Standby` with `Cluster Status: online` throughout — `ac1e103` holding for
a second run, on the state that produced it naturally rather than a hand-built one.

### Recovery

`group assign --group RealTest --node-id <node-3> --interface enX0` at 19:57:11 UTC returned rc=0
and the cluster rebalanced `81/82/0/82` → **`62/61/61/61`** within ~75s, `unique=245/245
missing=0 duplicated=0`, all four `Active`, cluster `online`.

Transient duplication during the moves: `duplicated=18` at T+25s, `1` at T+50s, `0` at T+75s, with
`missing=0` at every sample. Addresses are up on the destination before the source lets go and the
dedup pass converges behind the batch — no address was ever off the cluster during recovery.

### Errors, and what is not new

Two error classes appear in the run and **both predate this change**, measured on the old binary in
the 19:00–20:45 window: `failed to GARP. exit status 2` (61 on node-1) and
`ENFORCE: failed to release unassigned floating IP ... cannot assign requested address` (27 on
node-1, 31 on node-2). The second is the release pass racing its own inventory snapshot — the
address moved between the snapshot at the top of `enforceExpectations` and the bring-down — and it
is logged at error level for what is a no-op. It self-corrects (coverage was exact at rest), but
the widened release pass will hit it more often, and it is worth a defect of its own: re-check
existence at bring-down, or demote the message when the address is simply already gone.
That became **#41, now fixed — see its row in §5.**

### Leave-behind

`RealTest` = **245**, mode **active-active**, settled **`62/61/61/61`**, `unique=245 missing=0
duplicated=0`. `RealTest` reassigned to all four on `enX0` (`{'enX0': ['Management', 'RealTest']}`
on every node). All four running `70708a5fc74ccf59088ed72390dab67f`, verified via `/proc/pid/exe`.
`logging_level` = **info**. `/run/lbBootFlag` **absent on all four**, unchanged.

---

## Result 2026-07-29 (run 23) — #39 and #41 both VERIFIED FIXED LIVE; #42-#45 opened

Binary `e37965f3cfebdb645b20d4e3ef20bf1c` (code `573a3e6`), deployed and md5-verified on all four,
rolling restart one node at a time with the coordinator (node-2) last, from run 22's settled
`62/61/61/61` at `RealTest` = 245. Cluster stayed **active-active** throughout. Debug enabled on
all four first — which is how #42 was found. Baseline after the restarts was exact:
`configured=245 n1=61 n2=62 n3=61 n4=61 placements=245 unique=245 duplicated=0 missing=0`.

**Verify the running process, not the installed file — and pin the PID.** Run 22's lesson needed a
correction of its own: `md5sum /proc/$(pgrep -f /usr/local/sbin/pulseha)/exe` matches *several*
PIDs (the daemon plus the pgrep pipeline itself) and prints `Is a directory` / `No such file`
noise while still exiting 0. Use `systemctl show -p MainPID --value pulseha`. All four confirmed
on `e37965f3…` with post-deploy start times before any measurement was taken.

### #39: PASSES

Two 20-add bursts of freshly DAD-verified addresses (`arping -D` with the gateway and two cluster
IPs as positive controls; `10.200.1.173` came back LIVE and was excluded):

| Condition | Adds | Every rc | Per-add wall time | Total | Pre-fix comparison |
|---|---|---|---|---|---|
| node-4 stopped | 20 (`.144–.163`) | **0** | 0.037–0.069s | 12s | ~28s **per add** (~560s) |
| all four healthy | 20 (`.165–.185`) | **0** | 0.034–0.088s | 4s | ~13s per add (~260s) |

The negative control matters as much as the count, and it holds: no add came close to the 30s
deadline, so the deadline is no longer reachable and a zero-rc=1 run is meaningful rather than
merely lucky. The dead peer *was* waited on, off the request path — 60 `Sending request to bring
up` lines (20 IPs × 3 peers) with all 20 node-4 attempts failing `connection refused` while the
caller returned in 40ms. node-4 picked all 21 additions up from config within a minute of restart.

### #41: PASSES — and the mechanism is visible, not merely absent

Driving a genuine release storm took three attempts, which is itself the finding:

1. `unassign` alone released node-3's 71 but never reclaimed them → **#44**. Nodes 1/2/4 only ever
   gained addresses, so no still-assigned node had surplus to release.
2. `config set mode active-passive` applied to node-1 only → **#42**. No consolidation, no churn.
3. `unassign` + restart node-3 (run 22's actual sequence) → reclaim to `95/96/0/96`, then
   `assign` → rebalance back to `72/72/71/72` with transient `duplicated=14`. **71 addresses moved
   off three still-assigned nodes** — #41's exact make-before-break condition, at scale.

In that window, across all four nodes: **0** `ENFORCE: failed to release unassigned floating IP`,
where run 22 had 27 on node-1 and 31 on node-2. The Debug classification fired **201 times**
(15/80/77/29), so the races happened and every one was classified. Decomposing the 201 against the
31 `NETWORK: Unable to bring down IP … cannot assign requested address` Warn lines from inside
`BringIPdown`: **31** reached the kernel and lost the residual check-to-syscall race, **170** were
skipped before the syscall. Both halves of the fix — fresh inventory, live pre-check — are
separately observable live.

**Residual, honestly:** those 31 Warn lines come from `packages/network`, one layer below the fix,
which was deliberately left untouched. Error-level noise for a no-op is gone; warn-level noise for
the same no-op is not.

### The uncomfortable part: #39's fix exposes #43

Both bursts committed on node-1 and **did not propagate**: 286 against 267/268/268, stable for
135s+ with all four nodes healthy. This is not slow convergence — it is wedged, because the
periodic reconcile the broadcast defers to runs **only on the coordinator** (node-1: 0
`CONFIG_RECONCILE` lines in 5 minutes; node-2: 4). See the #43 row for the full mechanism.

This is a fair trade rather than a regression — the fix does what it claims, and the propagation
weakness is pre-existing — but it changes the operational advice. Before `bef7286`, each add's
13–28s cost accidentally rate-limited mutations below the broadcast's tolerance; runs 19 and 20
landed 244 adds that way. Now `add-ip` returns in 40ms and reports success for a change that may
exist on one node only. **#39 turned a false failure into a false success**, and until #43 is
fixed the safe pattern is to space bulk adds or verify propagation afterwards. Both bursts were
repaired by a single subsequent quiet `add-ip`, which pushed the whole backlog in one go.

### Other observations

- **#31 reconfirmed on a healthy cluster** — 56 of 60 fan-out RPCs refused, with only 1–2
  `Reconfiguring PulseHA server` events per peer, so one teardown refuses many seconds of traffic.
  It is the amplifier under both #37 and #43.
- **#45**: the bring-up path logs `file exists` at error level for an address already present —
  #41's mirror, and it escalates into `IP_FAILOVER: Some interfaces failed to bring up IPs`.
  **Now fixed — see its row in §5.**
- **A daemon restart no longer churns placement.** Restarting node-4 from a settled state left
  `72/72/71/72` untouched with `duplicated=0` — the kernel keeps the addresses across the restart.
  Run 20's `duplicated=73` came from restarting *mid-mutation*, not from the restart itself.
- **`journalctl -u pulseha` is the wrong query on these hosts.** The daemon logs to syslog
  (`log_to_syslog: true`), so the unit journal holds ~10 systemd lines and none of the
  application's; an early #39 fan-out check read 0 of everything from it. Use `-t pulseha`.
  Matching on a unique token (the test addresses) beats a time window — journalctl's displayed
  clock and the app's own timestamp disagree by an hour on these hosts.
- `pgrep -f /usr/local/sbin/pulseha` matches multiple PIDs; use `systemctl show -p MainPID`.

### Leave-behind

`RealTest` = **287** (245 + 41 added and DAD-verified free; `10.200.1.173` deliberately excluded as
LIVE), mode **active-active** on all four, settled **`72/72/71/72`**, `unique=287 missing=0
duplicated=0`, `RealTest` assigned to all four on `enX0`. All four running
`e37965f3cfebdb645b20d4e3ef20bf1c`, verified via `/proc/MainPID/exe`. `logging_level` restored to
**info** on all four individually (see #42 — one `config set` will not do it).
`/run/lbBootFlag` **absent on all four, untouched by this run** — as run 22 left it. It still gates
the appliance's config wipe, so restoring it will delete the 287 test addresses on the next daemon
start; that is a deliberate decision to defer, not an oversight.

---

## Fix 2026-07-30 — PR #227 review follow-up, defects #46-#54 (unverified live)

The second tranche from the PR #227 review. `157a2f9` and `9d2948b` took the three
merge blockers, `make testrace` and the trivial minors; this covers everything the
reply to the review deferred, plus the two things found while doing it.

**Nothing here has been verified live.** Every claim below is from code and unit
tests, so each row is marked "not verified live" — deliberately, because run 23's
lesson was that a fix consistent with a fix is weak evidence. The next whitecrane
run should treat #51 as the one to watch: it changes when reconciliation runs, not
merely what it does.

### What the review asked for

| Finding | Defect | Shape of the fix |
|---|---|---|
| `member_list.go` unlocked member reads | #47 | Snapshot each member's fields under its own lock, as `ConsolidationTarget` already did |
| `performPromotionAsync` bare `Status` writes | #48 | `SetStatus`/`MarkUnreachable`/`restoreMemberStates`; the bare reads in the same function too |
| `seedActiveActiveAssignments` lock contract | #49 | State the real contract; take `RLock` at the `ConfigSync` call site |
| Blocking RPCs in the 1s health-check loop | #51 | Single-flight goroutine off the tick, batched duplicate releases, bounded consolidation |
| `confirmPeerReleasedIPs` 10s deadline | #52 | `DemotionTimeoutFor` — 10s + 100ms/address, capped at 120s |
| `PlanMoves` transient over-capacity | #53 | `scheduleWithinCapacity` — trim and retry batches instead of reordering them |
| Duplicate survivor by record order | #54 | Local kernel state decides when the local node is a contender; record order otherwise |
| Envelope-only `ConfigSync` epoch write | #50 | Read and write the epoch/leader pair in one critical section |

### Found while doing it

- **#46, a hard self-deadlock.** `RemoveMember` holds the member list write lock and
  calls the exported `RedistributeIPs`, which takes it again. `sync.RWMutex` is not
  reentrant, so removing any node that still held addresses hung the daemon with the
  lock held. Not a review finding, and the fourth instance of this exact shape in the
  codebase (`RebalanceCluster`, `hasQuorumLocked`, #32's `Load()`/`Save()`).
- **#50 was wider than reported.** The review named the envelope-only branch; the same
  unsynchronised compare-and-write was also in the pre-lock epoch read and the
  apply-member-states block, so three of `ConfigSync`'s four paths, not one.

### Method

Every fix has a test that was **run against the unfixed code and observed to fail** —
in a throwaway git worktree at `HEAD`, so the working tree was never left in a
half-reverted state. What each one reported:

| Test | Against the old code |
|---|---|
| `TestRemoveMemberRedistributingDoesNotSelfDeadlock` | Times out, both branches |
| `TestRedistributeIPsSnapshotsMemberStateUnderLock` | `-race` at the three flagged lines |
| `TestConvergenceMetadataIsNeverObservedMismatched` | Mismatched pair **and** `-race` |
| `TestReconcilePassDoesNotBlockTheHealthCheckTick` | 2.0s block on a 1s tick |
| `TestReconcilePassesDoNotStackAndTheGuardIsReleased` | 4 passes where 1 is correct |
| `TestPlanMovesNeverTransientlyExceedsCapacity` | node2 holds 7 against capacity 4 |
| `TestDuplicateSurvivor{PrefersTheNodeHoldingTheAddress,DropsAStaleLocalRecord}` | Wrong survivor in both |

`#49` and `#52` are the two without a failing-first test: #49 is a contract and a lock
acquisition with no observable behaviour change, and #52's test pins the sizing rule
rather than a bug — the old constant is asserted to be insufficient.

### Left open, deliberately

- **#48's residual:** bare member-status writes elsewhere in `internal/server`
  (~lines 687, 1348, 4777, 4790). A wider pre-existing pattern the review did not
  scope; `health_check.go`'s writes were already correctly locked.
- **#54 cannot be closed** without an RPC that reports a peer's interface state — a
  proto change, which does not belong in a follow-up to an already-reviewed PR.
- The `partial_active` removal still needs calling out in the release notes as
  breaking, and the unauthenticated mutating RPCs still want their own issue. Both
  were acknowledged in the review reply as out of scope for this branch.

---

## Fix 2026-07-30 — defect #43 (lost propagation) and #31 (the listener cycling), unverified live

Two changes, both in `internal/server/server.go`, taken together because either alone
leaves the operator with the same problem.

**#43 — the broadcaster retries its own unpropagated config.** `broadcastConfigToPeersOnce`
gets four attempts across ~1.75s of backoff, and when they were exhausted it logged
`waiting for the periodic reconcile` and dropped the state. That reconcile runs only on
the coordinator (`HealthChecker.reconcileConfigAcrossPeers`), so a mutation taken on any
other node stayed local until the next successful mutation happened to carry the backlog:
run 23's node-1 sat at 286 against 267/268/268 for 135s+ with all four nodes healthy.
The broadcaster now records `{version, peers, attempts, backoff}` and wakes on its own
timer — 5s doubling to a 60s ceiling — clearing the record and logging
`CONFIG_PROPAGATION: every peer accepted the config` when it lands.

The coordinator gate on the *periodic* reconcile is untouched. It did not need relaxing:
the failing node is the one that knows it has an unpropagated version, so it is the right
owner of the repair, and #38's stamp makes its push safe (a peer holding something newer
answers `superseded config version ignored` and is dropped from the retry set).

**#31 — `Reconfigure` stops rebinding a listener that has not moved.** Every full
`ConfigSync` spawns `Reconfigure`, which tore the cluster listener down and bound a fresh
one regardless of whether the address changed. Under a burst that refused inbound RPCs on
every receiver for seconds at a time — 56 of 60 peer bring-up RPCs in run 23 — and starved
#43's in-pass retries. The address is now compared against what is being served, and the
teardown is skipped when they match.

**Method.** Both behavioural tests were run against unfixed code in a throwaway worktree at
`cfe38e3` and observed to fail: the #43 test reported `4 refused pushes, 0 accepted`, and the
#31 test reported the listener replaced. `TestReconfigureRebindsWhenTheBindAddressChanges`
passes at HEAD by design — it exists to stop the skip widening into "never rebind".
`make test` and `-race` over `internal/server` and `internal/membership` are clean.

**Nothing here is verified live.** Run 24 should watch, on a **non-coordinator**: a 20-add
burst propagating to all four nodes without a follow-up mutation; `CONFIG_PROPAGATION: every
peer accepted the config` appearing; and the count of `Reconfiguring PulseHA server` events
per peer during a burst, which should now be zero rather than 1–2. If #31's fix holds, the
#43 retry should rarely need to fire at all — both being observable separately is the point.

## Fix 2026-07-30 — defect #42 (`config set` is not cluster-wide) and #55, unverified live

Working tree on `active-active-mode-join`, over `d4408e9`. Three files changed
(`internal/server/server.go`, `internal/cli/config.go`, `packages/config/config.go`) and two
new test files.

**#42 — `config set` now knows the scope of every key it accepts.** The old handler wrote the
value into the local config, saved it and returned `updated`; nothing stamped a version and
nothing woke the broadcaster, so the change never left the node the CLI happened to run on
while the command's help promised "apply it to the cluster". Three separate outcomes now:

- **Cluster-wide keys** — `hcs_interval`, `fos_interval`, `fo_limit`, `auto_failback` — are
  stamped and broadcast through `markConfigDirty()`, the same path a group mutation takes, so
  they also inherit #43's retry rather than a single best-effort push. Note the three timing
  keys are read when the health checker *starts*, so the value now reaches every node but takes
  effect on each daemon's next restart — pre-existing, unchanged here, and now stated in the
  command's help rather than left to be discovered.
- **`mode` is delegated to `SetMode`.** Changing the mode is not a value to write: it
  consolidates the group onto one Active or seeds the active-active spread, and it
  re-broadcasts the member statuses that belong with the new mode. Writing the value past all
  of that is what produced run 23's wedged cluster — one node in active-passive logging
  `4 nodes are Active in active-passive mode; waiting for the coordinator to consolidate` 529
  times at a coordinator that was still in active-active. The delegation runs before
  `s.Lock()` is taken, since `SetMode` takes the same non-reentrant lock.
- **Logging and syslog keys stay node-local, and say so.** `ConfigSync` deliberately
  preserves them, so a peer left at debug for an investigation is not reset by the next
  broadcast — which also means a broadcast cannot carry them. They are applied locally and
  reported as node-local; the CLI prints the reach of every change and the help text lists
  which keys are which. Anything not in the table — `local_node`, `cluster_token` — is
  refused instead of written.

`SetMode` also marks the config dirty now. Its own broadcast is one unretried pass per peer
with the result discarded, so a peer that was briefly unreachable — a listener mid-rebind
under #31 is enough — stayed in the old mode indefinitely, and a cluster running two modes at
once is the split-brain configuration quorum exists to prevent. A peer that already applied
the direct send answers the re-push with `superseded config version ignored`.

**Two prerequisites underneath.** `Config.UpdateValue` used to leave a rejected value in the
live struct: the setter writes before `Validate` runs and only `Save` was skipped, so
`hcs_interval 500` returned an error *and* poisoned every subsequent `Save()` — including
saves belonging to unrelated operations — and with `config set` now broadcasting, the next
successful mutation would have carried the rejected value to the cluster. It rolls back on
both validation and save failure, and the error names the constraint instead of a bare
"invalid configuration value". **#55** is the second: `Load()` holds the config mutex and
`migrateConfig` persisted through the exported `Save()`, which takes it again, so loading any
config whose four syslog fields are empty hung the caller forever. Sixth instance of that
shape in the tree; fixed with a named `saveLocked()`.

**Method.** All seven new tests were run against `d4408e9` in a throwaway worktree and
observed to fail for the right reason — the peer never receiving `fos_interval=7000` or
`mode=active-passive`, `cluster_token` being accepted, `2 members Active … want exactly 1`
after the switch, the rejected `hcs_interval=500` left at 500 with the following `Save()`
failing, and `Load()` not returning within 10s. Two constants the tests reference were stubbed
in the worktree so the behavioural assertions could compile against the unfixed handler.
`make test` and `make testrace` both exit 0, no data races.

**Nothing here is verified live.** Run 24 should, from a **non-coordinator** node: run
`config set mode active-passive` and confirm all four configs flip and exactly one node ends
Active; run `config set fos_interval` and confirm it reaches all four; confirm
`config set logging_level debug` now says it applied to this node only, and still has to be
repeated per node; and confirm `config set cluster_token x` is refused. #55 needs no live
check beyond the daemon starting normally.

---

## Result 2026-07-31 (run 25) — #45 VERIFIED FIXED LIVE (one arm); #57 opened

Binary `c3b56a2a907cda6f9aa11a13f717356c` (code `5193ae5`), built with `make build`, deployed to
all four and md5-verified **on the running process** (`systemctl show -p MainPID --value pulseha`
→ `/proc/$MainPID/exe`) after a rolling restart in the order node-1, node-3, node-4, coordinator
node-2 last. Debug enabled per node first (#42 — it is node-local), `/run/lbBootFlag` removed on
all four beforehand and restored afterwards. Baseline from run 23's leave-behind was exact and
survived every restart: `configured=287 n1=71 n2=72 n3=72 n4=72 unique=287 duplicated=0
missing=0`, cluster **active-active** throughout.

**Provenance needed proving, not assuming.** The already-deployed binary contained both of #45's
new Debug strings, so it looked like the fix was live from run 24 — but its md5 matched no local
build of HEAD, and `pulsectl --version` read `development`/`unknown` (i.e. built with no ldflags),
so what was running could not be pinned to a commit. Deploying HEAD explicitly was cheaper than
arguing about it. Run 22's lesson, one step further: a string that proves the *fix* is present
does not prove *which build* is running.

### #45: PASSES on the arm that was exercised

The generator is run 22/23's sequence — `unassign` the group from a node, restart it (reclaim onto
the others), then `assign` it back (rebalance moves addresses off still-assigned nodes). Two rounds:

| Round | Node | Reclaim | Rebalance transient | Settled |
|---|---|---|---|---|
| 1 | node-3 | `95/96/0/96` | `duplicated=5` | `72/72/71/72` |
| 2 | node-4 | `95/96/96/0` | `duplicated=27`, `missing=11` | `72/72/72/71` |

Across the whole run window (five restarts, both storms), on all four nodes:

| Line | Run 23 | Run 25 |
|---|---|---|
| `IP monitor restore: failed to add addr` (Error) | 22 | **0** |
| `NETWORK: netlink.AddrAdd failed` (Error) | 9 | **0** |
| `ENFORCE: Failed to bring up IP on Active node` (Error) | 15 | **0** |
| `unable to bring IP up as netlink failed to do so` (cause string) | present | **0** |
| `IP monitor restore: expected IP was already back` (Debug, new) | n/a | **48** (7/9/22/10) |

The positive control is what makes the zeros mean anything, and it fired 48 times. The stronger
count is the correspondence: **every `file exists` string in the entire run was one of those 48
Debug lines** — the per-node `file exists` total equals the per-node Debug total exactly
(7/9/22/10) — so no EEXIST escaped to an error line, rather than merely no error line being
observed. Coverage settling exact after both storms is the outcome-level control: classifying
EEXIST as satisfied did not mask an address that was genuinely absent. A 60s quiet window after
settling logged zero classifications, zero warns and zero `file exists` on all four, so this noise
is churn-driven rather than steady-state.

**Only one of the two arms was exercised.** `NETWORK: IP was already up when adding it` — the
`network.BringIPup` arm — was **0** on all four, so that arm still rests on its unit tests. The
arm that was proven is the one that produced 22 of run 23's 31 error lines. A run that drives
bring-up through the `BringUpIP` RPC path specifically would close the other half.

### #57: the escalation #45 was blamed for is still there, for a different reason

`IP_FAILOVER: Some interfaces failed to bring up IPs` fired **7 times** on node-2. None came from
netlink: all 7 sit directly under `IP_FAILOVER: Failed to bring IPs up remotely … DeadlineExceeded`
and above `ACTIVE_CHECK: rebalance move failed count=23/24`. `bringIPsOnNodeUp` gives the remote
`BringUpIP` a flat 5s regardless of batch size, and a 23–24 address batch aimed at a node already
bringing up ~71 does not fit in it. Every one of those 7 "failed" moves had in fact landed. See the
#57 row; it is #52's defect on the bring-up side.

Worth stating plainly because it is the trap this run nearly fell into: the fix for #45 was
partly justified by clearing this exact line, and the line is still present. It is present for a
cause #45's fix does not claim to touch, which is only demonstrable by reading the line above it.

### Other observations

- **#41 holds for a second run.** Zero `ENFORCE: failed to release unassigned floating IP` on all
  four across both storms.
- **#41's warn-level residual scales with churn, and this run had far more of it.** 519 (node-2),
  501, 518, 534 `NETWORK: Unable to bring down IP … cannot assign requested address`, all from
  `packages/network`, against 31 in run 23. Not diagnosed here and not a new defect — the same
  documented residual one layer below the fix — but ~2000 warn lines for no-ops is now the loudest
  thing in a storm, and it is the noise that would hide a release that mattered. Zero in the quiet
  window.
- **#44 reconfirmed twice.** `unassign` released the node's addresses and the reclaim never fired
  until that node was restarted: `missing=72` for ~50s in both rounds.
- **#43 reconfirmed, with an operational consequence for this test.** `group assign` issued **on
  node-3** wrote node-3's config only; nodes 1, 2 and 4 still listed node-3 as `Management`-only
  60s later, so no rebalance could be planned and the storm did not happen. A later `assign`
  issued on node-1 propagated to all four within seconds and pushed the backlog — run 23's
  "repaired by a single subsequent quiet mutation", on a different command. Round 2's mutations
  were issued on the coordinator (node-2) and propagated within 8s. **Issue cluster mutations on
  the coordinator until #43 is fixed**, and verify propagation before concluding anything from the
  absence of churn.
- **A zero-error run with a zero positive control proves nothing.** The first scan of this run
  came back all-zeros — including the Debug classifications — because the reclaim alone never
  raced. That reads identically to a pass and is not one. The storm had to be driven properly
  before the zeros were worth reporting.

### Leave-behind

`RealTest` = **287**, mode **active-active** on all four, settled **`72/72/72/71`**, `unique=287
duplicated=0 missing=0`, assigned to all four on `enX0`. All four running
`c3b56a2a907cda6f9aa11a13f717356c` (`5193ae5`), verified via `/proc/MainPID/exe`.
`logging_level` restored to **info** on all four. `/run/lbBootFlag` **restored on all four**.

---

## Result 2026-08-03 (run 27) — #59 VERIFIED FIXED LIVE; #60 opened, and it blocks fixing #43

Binary `453aebae146f1dfad745d7bcdf7d2c0e` (code `0c2ad56`) on all four nodes, verified against
`/proc/MainPID/exe` after a rolling restart taken while the groups were empty. Mode
**active-active** throughout. Test group `VerifyTest` = `10.200.0.155-166` on `enX0`, all 12
DAD-verified free first with a positive control (gateway `10.200.0.1` plus two cluster IPs all
LIVE, so the probe was known good), assigned to all four nodes, settled at `3/3/3/3` with all 12
up and zero duplicates before every delete.

**#59 PASSES.** Six `group delete --force` runs, **zero stranded addresses in every one** — all
four nodes back to holding only their own `10.200.0.12N/23`. The original defect had node-1
keeping three addresses up indefinitely with no release pass ever running for them. Coverage of
both arms was deliberate:

- **Coordinator-driven deletes (5 runs).** Issued on node-2, the coordinator, so node-2 took the
  local netlink-verified path and nodes 1/3/4 took the peer RPC path. Node-2's log shows both
  halves: `Releasing floating IPs of a group being deleted node=MC-LB-node-N iface=enX0 count=3`
  once per node, and its own `RPC BringDownIP on iface enX0 for 3 IP(s)` for the local release
  through the handler. The local kernel check confirmed clean every time — a failure would have
  produced `floating IP(s) of this group are still up locally on enX0` and `Success: false`.
- **Simultaneous delete on all four (1 run).** Every node is then the local node for its own
  share, so this exercises four concurrent local paths instead of one plus three peers. Also
  clean.
- **Two runs stripped the group from every node's config within 5s of the delete**, which is the
  state a fixed #43 produces, removing the configured-but-unassigned fallback. Still zero
  strands — though see #60 for why that is weaker evidence than it looks.

The delete itself is fast: `rc=0` in **52ms** for a 12-address group across four nodes, the
release fan-out included.

**#60 opened — a peer's monitor restores what the release just removed, and the delete still
reports success.** Full diagnosis in the #60 row. Hit **2 of 6** runs. The decisive point for
sequencing future work: every run recovered via the peer's own surplus pass against a group that
was still configured there, because write 2 (the delete) never propagated — that is #43. Fix #43
without fixing #60 and a peer that restores its share then loses the group from its config has
those addresses outside every computable set, i.e. **#59 permanently, again**. Both ingredients
were observed in this run; only #43 kept them apart.

**#43 reconfirmed, unchanged and now load-bearing.** After the coordinator-only delete, node-2's
config listed `["Management","test"]` while nodes 1, 3 and 4 all still listed `VerifyTest` —
write 1 (dropping the assignments) propagated to all four, write 2 (the delete) did not.
Deleting on all four simultaneously converges, as the harness note already says.

**Method note worth keeping.** The first check of the settled baseline read `0` on node-1 and
looked like 3 addresses down cluster-wide; node-1's journal showed it bringing all three up
seconds later. The check had simply run before placement finished. A placement reading taken
without a settling loop is not a reading — poll until the total stops changing, which run 27 did
for every subsequent sample. Related: the setup transiently showed **17 of 12** addresses up
during the assign, which is the known make-before-break rebalance duplication, not a fault.

### Leave-behind

Groups `Management` (empty) and `test` (empty, `enX1`) only, identical on all four; `VerifyTest`
deleted everywhere. Mode **active-active**. Each node holds only `10.200.0.12N/23`, zero floating
IPs. All four running `453aebae146f1dfad745d7bcdf7d2c0e` (`0c2ad56`), verified via
`/proc/MainPID/exe`. `logging_level` **info** on all four (run 27 needed no debug — the release
lines are `Info`). `/run/lbBootFlag` **restored on all four**.

## Result 2026-08-03 (run 28) — #60 VERIFIED FIXED LIVE; the gate on #43 is cleared

Binary `188c2d9b18838360507fc7f5e496d038` (code `671ec04`) on all four nodes, verified against
`/proc/MainPID/exe` after a rolling restart taken while the groups were empty. Mode
**active-active** throughout. Test group `VerifyTest` on `enX0`, recreated from scratch for every
cycle: `10.200.0.155-166` (12 addresses, settles `3/3/3/3`) for runs 1-10 and the re-add check,
`10.200.0.155-190` (36 addresses, settles `9/9/9/9`) for runs 11-13. All 36 DAD-verified free
first with a positive control.

**#60 PASSES. 14 `group delete --force` cycles, zero strands and zero restores in every one.**
`expected IP removed from Active node; restoring` — the defect's signature, seen in 2 of run 27's
6 cycles — is **0 on all four nodes in all 14 cycles**. Every cycle had the group's addresses gone
cluster-wide at +12s and still gone at +42s.

**The gate is proved to have fired, not merely to have gone unneeded.** Run 27's rate was ~1 in 3,
so all-zeros against a silent positive control would have been indistinguishable from the race not
landing. The suppression lines appear in **4** of the cycles:

| cycle | group size | node | `expected IP was released on request; not restoring` | `ENFORCE: not restoring floating IPs this node was told to release` |
|---|---|---|---|---|
| 1  | 12 | node-3 | 1 | 2 |
| 11 | 36 | node-1 | 6 | 6 |
| 13 | 36 | node-1 | 7 | 9 |
| 13 | 36 | node-3 | 1 | 1 |
| re-add check | 12 | node-3 / node-4 | 3 / 2 | — |

Both consumers of the shared `restorableIPs` decision are therefore exercised live — which is the
coverage that matters, since the wiring lives in `ip_monitor_linux.go` and the unit tests cannot
reach it (the same residual as #58). The pre-fix sequence is intact right up to the point it
diverges; node-3, cycle 1:

```
RPC BringDownIP on iface enX0 for 3 IP(s)
Removed IPs from interface iface=enX0 remaining="[10.200.0.162/23 10.200.0.164/23]"
Removed IPs from interface iface=enX0 remaining=[10.200.0.161/23]
IP monitor: expected IP was released on request; not restoring ip=10.200.0.161/23 iface=enX0
ENFORCE: not restoring floating IPs this node was told to release count=3
```

**Widening the group is what makes #60 reproducible on demand.** At 12 addresses the race arose in
**1 of 9** cycles; at 36 it arose in **2 of 3**. A larger write 1 takes the peer longer to apply,
and that lag is precisely the window — so size the group up rather than repeating identical small
cycles when hunting anything in this family.

**The 60s backstop does not block a legitimate re-add.** Checked deliberately, because a protection
keyed on a timer could just as easily delay a real re-assignment. The group was re-created ~26s
after a delete, inside the window: node-3 had been told to release `10.200.0.158` at 14:28:38 and
brought it back up at **14:29:03**, 25s in. `AddExpectedIPs` clears the record as designed. Note
that cluster-wide coverage returned over ~45s rather than at once — that is ordinary placement
latency behind a 12-address `add-ip` loop, and it looks exactly like a 60s lapse if read off the
total. Judge this on per-address bring-up timestamps, never on the aggregate.

**#43 reconfirmed in passing.** After cycle 10's coordinator-only delete, node-2 held
`["Management","test"]` while nodes 1/3/4 still listed `VerifyTest` — write 1 propagated, write 2
did not, unchanged from run 27. Cleanup needs the delete issued on all four simultaneously.

### Method notes

- **The `arping -D` free-range check must read the exit code, not the output text.** Grepping for
  `Received 0 repl` matches nothing — the wording is `Received 0 response(s)` — so every address
  reads as in-use, including a range that is entirely free. `arping -D` exits **1** when the
  address answers and **0** when it does not. The positive control (gateway plus two cluster IPs)
  did not catch this, because a classifier that can only ever return "in use" returns it for the
  controls too. A control only works if the failure mode it is meant to catch would change its
  answer.
- Journal collection used `journalctl --show-cursor` / `--after-cursor` per cycle rather than
  `--since`, which sidesteps both documented clock traps on these hosts (node clock behind the
  workstation, journalctl display vs app timestamp an hour apart).

### Leave-behind

Groups `Management` (empty) and `test` (empty, `enX1`) only, identical on all four; `VerifyTest`
deleted everywhere. Mode **active-active**. Each node holds only `10.200.0.12N/23`, zero floating
IPs. All four running `188c2d9b18838360507fc7f5e496d038` (`671ec04`), verified via
`/proc/MainPID/exe`. `logging_level` **info** on all four (the suppression lines are `Info`).
`/run/lbBootFlag` **restored on all four**. `10.200.0.167-190` DAD-verified free 2026-08-03 and
now available alongside `155-166` for a 36-address group.

## Fix 2026-08-03 — defect #43, the receiving half: `ConfigSync` could not represent a removal

`internal/server/server.go`, plus `internal/server/config_deletion_test.go`. This is the second
#43 fix; the 2026-07-30 one (the broadcaster's retry timer) was necessary and stands, but it
addressed the arm where a push *fails*. This arm is the one where the push **succeeds and is then
discarded by the receiver**, which is why runs 27 and 28 both reconfirmed #43 on binaries that
already carried the retry.

**The mechanism, in one line: absence and emptiness are what a removal looks like on the wire, and
the merge read both as "this sender has no opinion".**

```go
// before — for every local group, and again per node interface
incomingIPs, ok := newConfig.Groups[g]
if !ok || len(incomingIPs) == 0 {
        newConfig.Groups[g] = copyOf(localIPs)   // resurrects whatever the sender deleted
}
```

All three removing mutations produce exactly that shape:

| mutation | what it leaves in the config | how the receiver read it |
|---|---|---|
| `commitGroupDeletion` (`group delete`, write 2) | key deleted from `Groups` | `!ok` → restore local copy |
| `UnassignGroupFromNode` (last group on an interface) | interface key deleted from `IPGroups` | `!ok` → restore local assignment |
| `RemoveIPFromGroup` (last address) | group present, list empty | `len == 0` → refill from local |

And the receiver answered `Success: true` throughout, because from its point of view nothing had
gone wrong. So the sender's `broadcastConfigToPeersOnce` deleted the peer from `pending`, found
`pending` empty, called `clearUnpropagated()` and logged `CONFIG_PROPAGATION: every peer accepted
the config`. **The retry could not fire, because no failure was ever reported.** A silent
divergence that reports success on both ends is strictly worse than a loud one, and it is why this
survived a fix aimed at the same defect number.

**The fix.** In the full-config branch, a payload that carries the field is authoritative about it,
including about what is no longer in it. Preserve-local now triggers only when the map is **nil**:

```go
if newConfig.Groups == nil && len(s.config.Groups) > 0 { /* deep copy local */ }
if nIncoming.IPGroups == nil && len(existing.IPGroups) > 0 { /* deep copy local */ }
```

Two things make that keying correct rather than merely narrower:

- **nil is a real signal here, and emptiness is not.** `floating_ip_groups` and `group_assignments`
  carry no `omitempty`, and `config.New`/`Load` always initialise the maps, so any live daemon
  sending a full config emits at least `{}`. "I have no groups" therefore stays distinguishable
  from "I do not speak groups", which is the case the merge was written for (the #5-era envelope)
  and the only one that still wins against local.
- **Absence is now ordered on the same clock as content.** This code runs past two stamp checks,
  the second re-taken under `s.Lock()` precisely because a local mutation can land in the
  unmarshalling window (#38). A removal is only ever honoured from a payload strictly newer than
  what this node holds — the same guarantee a newer config already relied on to replace a group's
  entire address list wholesale.

**Deliberately out of scope: node-level absence.** A node missing from an incoming config is still
kept entirely, so a stale or partial payload cannot delete peers, and `leave`/`RemoveMember`
propagation is untouched. The observed #43 instances are all group-shaped.

**Expected consequence for #59 and #60.** Run 27 recorded that a propagating delete is what turns
#60's restored share into a permanent strand, and #60 was fixed and verified first for that reason
(run 28, gate observed firing in 4 of 14 cycles). This fix is what makes deletes actually
propagate, so **run 29 is the run where that pairing is tested for the first time** — the ordering
those two rows describe is no longer hypothetical.

### What run 29 should watch

- **The delete propagating at all**, which is the fix itself: `group delete --force` on the
  **coordinator only**, then read `floating_ip_groups` on the other three. They should lose the
  group within seconds; runs 27 and 28 had them keep it indefinitely. This is the positive control
  for everything below — if the group still lingers, nothing else in this section is being tested.
- **`group unassign` and the last-address `group remove-ip` propagating**, the two arms never
  exercised live. #44 (`unassign` releases but never reclaims without a restart) was diagnosed
  against a cluster where the unassign itself never left the node, so **re-diagnose #44 from
  scratch** rather than assuming the old finding holds.
- **#59 and #60 together, which is the risk this fix creates.** A peer that restores its share
  (#60's window) and then loses the group from config (now that write 2 arrives) would have those
  addresses outside every computable set. #60's 60s release protection is what should prevent it.
  Use a **36-address** group — run 28 showed the race arises in 2 of 3 cycles at 36 against 1 of 9
  at 12 — and grep all four for `expected IP removed from Active node; restoring` (want 0) and for
  strands after the group is gone.
- **A burst of adds still converging**, since the merge is on the path of every full sync: 20
  `add-ip` calls on a non-coordinator, then all four configs equal, with
  `CONFIG_PROPAGATION: every peer accepted the config` and no `peers did not accept the config`.
- **The remaining arm of #43, which this does not fix and which is by prior decision:** a mutation
  taken on a node whose in-memory stamp is behind (any node between its restart and its first full
  `ConfigSync`) is correctly rejected by peers as superseded and then reverted, while the CLI has
  already reported success. The sender does log it —
  `CONFIG_BROADCAST: peer holds a newer config; this node's change will not propagate` — so it is
  visible rather than silent. This is the documented cost of an in-memory-only stamp (#3 makes a
  persisted one wrong exactly when it matters), and blocking the reply on propagation was rejected
  for undoing `bef7286`. **Operationally: after restarting a node, do not issue mutations on it
  until it has taken a full `ConfigSync`.** Run 25's `group assign` on node-3 is this arm, not the
  merge arm.

---

## Result 2026-08-03 (run 29) — #43 VERIFIED FIXED LIVE at last; #59/#60 hold beside it; #44 no longer reproduces; #61 opened, fixed and verified

Two binaries, both md5-verified on all four via `/proc/MainPID/exe`:
`9ad384909eba9c1a1ac00665d758c819` (HEAD `8aa67db`, cycles 1–4) and
`50eab7ff3c596f04e1a1420f17390477` (the #61 fix, cycle 5). Mode active-active. Test group
`VerifyTest` = `10.200.0.155-190` on `enX0` — **36 addresses**, all DAD-verified free by exit code
with a live positive control (gateway + two cluster IPs all `IN-USE`), assigned to all four,
settled `9/9/9/9` before every delete. Six `group delete --force` in total, all issued on the
**coordinator only** (node-2, `049-b22-093-2d3` — the lowest healthy node UUID, which is
`clusterCoordinator`'s rule and cheaper to compute than waiting for a re-broadcast line to appear).

### #43 PASSES — the first time a `group delete` has ever propagated on this cluster

**Six deletes, six times the group left all four configs.** Every cycle: `has_group=true` on all
four before, `has_group=false` with `assignment_entries=0` on all four after, `rc=0` in 1–2s. The
positive control is the pre-delete reading — the group is proven present everywhere first, so its
absence afterwards is propagation and not an artefact of it never having arrived.

Runs 25, 27 and 28 all reconfirmed #43 on binaries that already carried the 2026-07-30 broadcaster
retry. The receiving-half fix (`53d6506`) is the one that mattered, exactly as its commit message
predicted: the merge could not *represent* a removal, so every peer resurrected the deleted group
and answered `Success: true`.

### #59 and #60 both hold with deletes propagating — the risk the #43 fix created

This was the stated reason run 29 existed: once deletes propagate, a peer that restores its share
inside #60's window and *then* loses the group from config has those addresses outside every
computable set, which would be #59 permanently.

- **#59: zero strands in all six.** Post-delete kernel state `0 | 0 0 0 0` every time.
- **#60: zero `expected IP removed from Active node; restoring` in all six**, on all four nodes.
- **#60's protection was observed doing the work, not merely idle.** Both consumer arms fired:
  cycle 1 node-1 (enforce ×2), cycle 2 node-1 (watcher ×2, enforce ×8), cycle 4 node-1
  (watcher ×6, enforce ×6) and node-4 (watcher ×2, enforce ×3). The window opened repeatedly and
  the 60s record is what kept it harmless.

### #44 no longer reproduces — and the cause was **#58**, not #43

Re-diagnosed from scratch, 36 addresses, `unassign` from node-4 issued on the coordinator:

| | run 23 / run 25 | run 29 |
|---|---|---|
| unassign propagates | yes (run 23 recorded it explicitly) | yes, all four in <20s |
| unassigned node releases | yes, 6s | yes |
| **remaining nodes reclaim** | **never, until that node was restarted** (`missing=71` for 8 min) | **all 9, within 15s** |

Settled `12/12/12/0` at t+15s and held it for the full 3-minute watch, cluster total still **36 of
36** — no strand, no loss, no restart needed.

**The run-29 brief's premise for this was wrong, and the correction is the useful part.** #44 was
*not* an artefact of the unassign failing to propagate — run 23's own entry records it reaching all
four `group_assignments` within seconds. The orphan detector
(`health_check.go:1203`, `orphanedGroupIPs` over configured groups vs `hosted`) already existed in
run 23 and logged *nothing*, because **`hosted` is built from each member's `ActiveIPs`, and before
#58 a node's released addresses were never dropped from its own assignment list**. The released
addresses therefore still counted as hosted, `orphanedGroupIPs` returned empty, and the reclaim
correctly declined to fire on a set it could not see. That is precisely run 23's symptom —
"71 configured addresses on no node at all… zero reclaim, vote or capacity lines."

So #58 (`6d55cd8`, verified live run 27) fixed #44 as a side effect, and today's evidence is the
mechanism running end to end on the coordinator: `Initiating vote for redistribution of 8 IPs` →
voting session → `ACTIVE_CHECK: redistributing 8 orphaned floating IP(s)`. Zero such lines existed
in run 23. **This is inferred from the mechanism, not from an A/B against run-23 code** — but it
fits both records and #43's propagation was never the missing ingredient.

Worth keeping as a pattern: **a defect whose diagnosis blames a mechanism can be a symptom of a
bookkeeping bug one layer down.** #44 looked like "the reclaim is broken"; the reclaim was working
correctly on a false inventory.

### NEW #61 — an already-released address reported as a failed bring-down (found, fixed, verified live in the same run)

**23 `ERRO … BringDownIP failed … cannot assign requested address` lines across the three cycles on
HEAD** (1, 14 and 8), on node-2, node-3 and node-4.

Mechanism, from node-2's cycle-2 journal, all inside one second: the delete's release fan-out calls
`RPC BringDownIP … for 9 IP(s)`, and the node's own enforce pass concurrently logs
`ENFORCE: releasing floating IPs this node is no longer assigned count=8`. Two independent release
paths, same node, same addresses. The enforce pass is #41-hardened — it pre-checks `stillHeld`,
re-checks after a failure, and classifies a vanished address as a Debug no-op — so it completes
silently. `Server.BringDownIP` had **no classification at all**, so its per-IP loop then hit
EADDRNOTAVAIL on the 8 the enforce pass had already released and logged one Error each. Exactly
#41's shape and #45's mirror, on the one release path neither fix covered.

Bounded to noise: the RPC returns `Success: true` with `"Best-effort: some IPs may not have been
present"` either way, so nothing acted on the false failures. But it is the noise class that hid
the #59/#60-era failures, and now that deletes propagate it fires on **every** delete.

**Fix:** `network.AddrDelSatisfied` mirroring `AddrAddSatisfied`, keyed on EADDRNOTAVAIL through
`errors.Is` (the errno may arrive wrapped in the kernel's extended-ack message; `unix.EADDRNOTAVAIL`
is itself a `syscall.Errno`, so one comparison covers both types), with a live not-held check for
any other failure. **The inversion is the point:** for a delete the wanted state is the address
being *absent*, so `heldByTarget` answering false is success. An address still up after a failed
delete is still a failure. Unit tests in `packages/network/addr_del_test.go`; a classifier with the
old behaviour was confirmed to misclassify both no-op cases while already getting the
genuine-failure case right, so the fix loosens nothing that mattered.

**A trap that cost a cycle and is worth inheriting: `packages/network` cannot emit a Debug line at
all.** The first fix logged the no-op via that package's `log.Debug`, and cycle 4 came back with
**zero** `BringDownIP failed` *and* **zero** classifications — an all-zeros pass with a dead
control, indistinguishable from the race simply not happening. The cause is structural, not
configuration: `packages/network` logs through charmbracelet's *package-level* logger, which
`NewWithOptions` leaves at Info and which **nothing in the tree ever calls `SetLevel` on**. The
daemon's own logger is a separate instance that `main.go` levels from config. So a Debug line in
`packages/network` is unreachable at any `logging_level`, while its `Warn`/`Error` render fine —
which is why the *symptom* was always visible and the *fix* never could be. Hence
`BringIPdownClassified`, which returns `alreadyGone` so `Server.BringDownIP` can log it on the
daemon logger where an operator can turn it up. (This does not undermine #45's run-25 verification:
that entry records the `network.BringIPup` arm as never having raced and unit-tests-only, and its 48
Debug classifications as the `IPMonitor.restoreIP` arm — the daemon logger.)

**Verified live, cycle 5, with the control firing:** node-2 `ENFORCE: releasing floating IPs … `
released 3, the RPC then classified those same 3 (`BringDownIP: IP was already down` ×3), and
`BringDownIP failed` = 0 and `NETWORK: Unable to bring down IP` = 0 on all four. Pre-fix that same
overlap produced one Error per address.

### Other observations

- **#33 re-observed** during the #44 reclaim: `ERRO failed to GARP. exit status 2`, twice each on
  node-1 and node-3. `SendGARP` execs `arping -U`, which exits 2 for an address the node does not
  hold — a GARP announced against a stale set. Not new, not diagnosed further here.
- **#42's honesty half confirmed live** (it was fixed 2026-07-30 and never verified): `config set
  logging_level debug` now reports *"applied to this node only — this key is node-local and has to
  be set on each node"* on every node, instead of claiming a cluster-wide apply.
- **The coordinator is computable, so don't wait for a log line to find it.**
  `clusterCoordinator` picks the lowest ID among healthy members, so
  `jq -r '.nodes|keys' | sort | head -1` answers it instantly. The
  `re-broadcasting config from the coordinator` line only appears when there is an unpropagated
  config to re-push, and grepping a quiet 30-minute window for it returns 0 on all four — which
  reads exactly like "no coordinator".

### Leave-behind

Groups `Management` (empty) and `test` (empty, `enX1`) on all four, consistent; **zero floating IPs
held anywhere**; mode active-active; `logging_level` back to `info` on all four; `/run/lbBootFlag`
**restored on all four**; all four running `50eab7ff3c596f04e1a1420f17390477`. `10.200.0.155-190`
were DAD-verified free at the start of this run and are free again at the end.

### What run 30 should watch

- **#57** is now the oldest open defect with a known diagnosis (flat 5s deadline in
  `bringIPsOnNodeUp`, `server.go:~6232`) and it is what still produces
  `IP_FAILOVER: Some interfaces failed to bring up IPs` — read the cause line above that escalation.
- **#51** remains the PR-#227 fix flagged as the one to watch; none of #46–#54 has been verified
  live, and #44's disappearance is a reminder that unverified fixes can be silently load-bearing.
- **#35/#36 (capacity unenforced)** are untouched since run 18 and are the natural next target now
  that placement, release, delete and reclaim all behave.
- The 36-address group at `10.200.0.155-190` settling `9/9/9/9` is a good harness: a full
  create/assign/settle/delete cycle takes ~4 min and the release races reproduce readily at that
  size.

## Result 2026-08-03 (run 30) — #33 VERIFIED FIXED LIVE; the release-path family holds at 8x scale

Binary `0d521377eaee1cc49302f34785b3a53c` (`ea988bc`) on all four, md5-verified against
`/proc/MainPID/exe`. `logging_level=debug` on all four with `heartbeat convergence nudge` as the
"Debug is really on" control (11-20 per node per minute). Mode active-active. Group `Run30` =
**288 addresses**, `10.200.0.152-255` + `10.200.1.0-186` less `.1.101`, `.1.103`, `.1.173`, on
`enX0`, assigned to all four.

- **#33 PASSES.** `failed to GARP. exit status 2` = **1** cluster-wide (node-4), against run 17's
  **173** on a *smaller* group. The positive control fired **7 times on all four nodes** and
  suppressed **342** announcements of addresses the interface no longer held. The one survivor is
  the documented check-to-syscall residual, not a miss. Detail is in the #33 row above.
- **The direction of the switch decides whether the defect can appear at all.** The
  active-active -> active-passive consolidation scored `GARPfail=0 skip=0` with the announce path
  confirmed running (`bringups=2` on node-2) — because node-2 *kept* all 288 addresses it announced,
  so no entry in its set could go stale. Only the switch back produces the churn. A run that tests
  the consolidation direction and reports zeros has proved nothing.
- **The churn that makes the set stale, at 1-3s sampling:** node-3 went 72 -> 19 -> 7 -> 17 -> 60
  inside 25s. Every skip line lands in exactly that kind of window.
- **Coverage transient, worse than any previously recorded:** `unique` fell to **145 of 288** —
  143 addresses down cluster-wide — and node-4 sat at **0** for ~70s before its share arrived. The
  #29 family, an order of magnitude larger than run 15's 56-for-2s, and it is a real outage window,
  not duplication. Settled `72/72/72/72`, `unique=288 dup=0`, stable 4 min.
- **A plateau is not convergence.** A settle detector keyed on "4 identical samples" declared
  `72/122/60/0 unique=254` settled — 34 addresses down, node-4 empty. The coordinator's next pass
  fixed it 10s later. Require the *expected* distribution, or poll well past the first plateau.
- **The release-path family holds at 8x the address count it was verified at.** The teardown was a
  single `group delete --force` of 288 addresses on the coordinator: propagated to all four configs
  (**#43**, seventh consecutive run), **zero strands** on any node (**#59**), and every noise class
  at zero — `BringDownIP failed` = 0 (**#61**), `cannot assign requested address` = 0 (**#41**'s
  warn residual), `ENFORCE: failed to release` = 0 (**#34**'s enforce half), `expected IP removed
  from Active node; restoring` = 0 (**#60**) with its `not restoring` control firing 6x on node-1.
  Previous verification of this family was at 12 and 36 addresses.
- **#8 stays fixed at 288 addresses:** both mode switches returned rc=0 in 29s.
- **#37's cost is gone from the add path when the group is unassigned.** Building the 288-address
  group took **8 seconds** (~30ms per `add-ip`), because the `#39` fix places a new address on
  exactly one node and fans out asynchronously — and with the group assigned to nobody there is no
  owner to place it on at all. Run 19's ~13s-per-add figure does not apply to this shape. Create
  and populate the group *before* assigning it: it is three orders of magnitude cheaper.

**Method traps hit this run.**
- **A journal cursor that does not seek returns 0 lines and looks exactly like a clean window.**
  Capturing the cursor with `grep -o "s=.*"` off `-o cat` mangled node-4's, and
  `--after-cursor` answered `Failed to seek to cursor: Invalid argument` on stderr while printing
  nothing to stdout — so the count read `GARPfail=0 lines=0` and would have been reported as a pass.
  Take the cursor as `journalctl -n1 -o json | jq -r .__CURSOR`, and **verify every cursor seeks
  before trusting a count taken with it**.
- Nodes 2/3/4 share a journal seqnum-id (`s=626af24f…`, cloned VMs), so identical cursor prefixes
  across nodes are not evidence of a capture bug — the `i=` and `b=` fields are what differ.
- `pulsectl group create` takes the name **positionally** (`group create Run30`), not `--group`;
  the mode command is `pulsectl cluster mode set --mode …`, not `pulsectl mode set`. There is no
  `/usr/bin/time` on these hosts.
- **Two addresses inside the old `RealTest` range are now live to something else** —
  `10.200.1.101` and `10.200.1.103` both answered `arping -D`, alongside the known `10.200.1.173`.
  The recorded free-range scan is always point-in-time; re-sweep every run. 288 of 291 probed free,
  with the gateway and two cluster IPs as INUSE controls.
