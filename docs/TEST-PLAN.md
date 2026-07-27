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

| Case | Defect | Status |
|------|--------|--------|
| TC-1, TC-8 | #1 two-Active in active-passive | Fixed `5b1e6bf`, needs live re-verification |
| TC-2 | #6 Active self-strips VIPs on Unknown | Fixed, needs live verification |
| TC-3 | #5 config diverges under concurrent mutation | **Open** |
| TC-4, TC-5 | #4 serial 4s GARP per IP | **Open** — quantified 2026-07-26: 13m49s to fail over 201 IPs |
| TC-6 | #2 active-active — **split-brain, not thrash** | **Open, blocker.** Every node claimed all 203 IPs; live VIP triple-homed; manual recovery |
| TC-6 | #7 GARP starvation → false death of the Active | **Open, root cause of #2.** Re-run 2026-07-27 refined this: the trigger is a *promotion election*, not orphan reclaim |
| TC-6 | #12 promotion never demotes an *absent* incumbent (`shouldDemote=false`) | **Open, the core split-brain defect** — 103/201 IPs dual-homed |
| TC-6 | #13 failed `assign IPs` RPCs are never retried | **Open** — nodes 3 and 4 got 0 and 1 IP; 100 planned placements dropped |
| TC-6 | #14 non-Active branch strips *all* group IPs, not just unassigned | **Open** — 92/201 addresses down cluster-wide at once |
| TC-6 | #15 GARP re-announces addresses that never moved | **Open** — 200s for 50 already-present IPs; amplifies #4 |
| TC-6 | #16 `BringUpIP` swallows "IP already present" as success | **Open** — free split-brain detector currently discarded |
| TC-6 | #8 `SetMode` unabortable — blocks on `s.Lock()` | **Open** — no CLI escape from a bad switch |
| TC-6 | #9 `ConsolidationTarget` selects a node with a dead daemon | **Open** |
| TC-6 | #10 `Management` group redistributed like any other | **Open** — spreads live VIPs |
| TC-6 | #17 the mode change is lost entirely if `SetMode` is interrupted | **Open** — cluster ended in active-passive; no retry, no propagation |
| TC-7 | capacity enforcement | **Not runnable** — requires active-active, which cannot be entered (TC-6) |
| TC-8 | return to active-passive | **Not runnable as specified** — the cluster returns to active-passive *by itself* after a failed switch. Its pass condition (one Active holding all group IPs, others zero) was met at 00:37, but via the failure path, not via `mode set` |
| TC-8 | #27 reverse switch runs two whole-group consolidations onto two different targets | **Fixed, unverified live** — `SetMode` now propagates mode + states in one ConfigSync before moving any address. See "Root cause (defect #27)" below |

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
- **Not the monitor re-widening its expectations.** The per-tick recompute at
  `ip_monitor_linux.go:230` is gated on active-active, so in active-passive the
  cached set is used and a demoted peer's stale expectation is its old ~50, not the
  whole group. The whole-group claim comes from the consolidation path only.

**Fixed:** `SetMode` now sends the config and the member states in a single
ConfigSync, before any address moves. `ConfigSync` already recognises a full config
by its `pulseha` root and reads `member_states`/`epoch`/`leader_id` off the same
object, so one payload carries both and no proto change was needed. Peers therefore
leave active-active and learn their new status together, which stops a second node
acting as coordinator at all. Demoted peers also now release from their own IP
monitors as soon as the message lands, rather than waiting on the serial
`BringDownIPs` calls. Unit test: `TestConfigAndStatePayloadCarriesModeAndStatesTogether`.

**Not yet verified live** — needs a TC-8 pass on whitecrane, and the cluster must be
rebuilt and redeployed first (it is still running the pre-dependency-upgrade binary).
Two things to watch on that run:
- The target is now promoted in the same message that demotes the others, so the
  monitor-driven bring-up on the target can overlap the monitor-driven release on
  the demoted nodes by up to one enforce tick. `SetMode`'s own explicit IP work
  still runs releases-before-activation, so the overlap is bounded by a tick rather
  than by the length of the switch — but confirm the peak `duplicated` is small
  rather than assuming it.
- Peers now receive a full config mid-switch, which triggers their `Reconfigure()`
  and restarts their gRPC listener, so node-1's subsequent `BringDownIPs` RPCs may
  fail transiently. That is survivable by design — the demoted peer strips the group
  itself once it knows it is Passive — but the release should be confirmed against
  `ip addr`, not the absence of an error.

The deeper fragility is untouched and still worth its own entry: **`SetMode`
consolidates from whichever node received the CLI request, which need not be the
coordinator, while `enforceSingleActive` restricts the same decision to the
coordinator.** Two deciders for one invariant will disagree again whenever their
member maps differ. Making the switch delegate consolidation to the coordinator
would collapse them into one, and is the better long-term shape.
