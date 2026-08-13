# Runbook — verify defect #38 live on whitecrane (run 20)

Verifies commit `e2c7143` (Lamport config version). Written 2026-07-28.

> **EXECUTED 2026-07-28 — PASS. See `TEST-PLAN.md` "Result 2026-07-28 (run 20)".**
> Two corrections this runbook got wrong, kept here so the next run does not repeat them:
> 1. **"Pause node-2's daemon" as SIGSTOP is NOT the sharper test.** It preserves
>    `configVersion`, and the peer is repaired by broadcast retries within ~1.5 min — before its
>    once-a-minute reconcile fires — so it never holds stale content to push. A 3m19s pause
>    produced only an equal-version node-ID-tiebreak rejection. **Use `systemctl stop`/`start`**:
>    the node boots behind, and its reconcile is rejected on a genuine `version < held`.
> 2. **§6's "an add that returned non-zero was never claimed to have succeeded" is wrong** — see
>    defect #39. Two adds exited 1 with `DeadlineExceeded` and were applied on all four anyway.
>    A non-zero add cannot be excluded from the expected count; check the addresses, not the
>    exit codes.

**What #38 is:** an `add-ip` that logs `Successfully added IP …` is later erased from *every*
node's config. Uniform loss, not divergence — so **TC-3's "all four agree" criterion cannot detect
it**. The check must be *"the configured count equals the number of adds issued"*.

Pre-fix rate was 9/200 ≈ 4.5%, so a 40-add batch expects ~2 losses without the fix, 0 with it.
A clean run is suggestive, not conclusive, at that sample size — see "If it passes" below.

## 0. Preconditions

- Cluster left by run 19: `RealTest` = 201 addresses, mode **active-active**, settled `51/50/50/50`.
- **node-2 is the coordinator** (lowest UUID `049…`) and is the node that re-broadcasts. It was
  left on `logging_level=debug`.
- Node IPs: see the `test-cluster-whitecrane` memory. **SSH must be forced to IPv4** (`-4`) — the
  names have AAAA records with no route.

## 1. Build and deploy

`deploy.sh` installs binaries only — it does **not** restart the daemon.

```bash
cd /Users/matthewcooper/Development/Load-Balancer/pulseha
make build && make cli
md5sum cmd/pulseha/bin/pulseha            # record; verify against each node after deploy
DEPLOY_HOSTS='<ip1> <ip2> <ip3> <ip4>' ./deploy.sh
```

Do **not** build with `-mod=mod` — it rewrites `go.mod`/`go.sum`.

## 2. Enable debug on node-1 as well as node-2

Log level is applied **only at startup** (`cmd/pulseha/main.go:77`) and is preserved across
ConfigSync, so it must be set per node in `config.json` and that node restarted.

Set `pulseha.logging_level = "debug"` on **node-1** (the mutator) and confirm node-2 still has it.

**Control first:** grep for `heartbeat convergence nudge` (fires every 3 health checks). If that
line is absent, Debug is off and no other absence proves anything. This trap produced a wrong
conclusion in run 19.

## 3. Rolling restart — one node at a time, passives first, Active last

Never cold-start all four (defect #23: two nodes each claim the whole group). A rolling restart
preserved the baseline exactly in run 19.

Confirm `51/50/50/50` and `duplicated=0` before proceeding.

## 4. Baseline the config count on all four

```bash
# on each node
sudo jq '.floating_ip_groups.RealTest | length' /etc/pulseha/config.json
```

All four must read the same number. Record it as `N0`. Defect #5's history says never trust a
baseline without checking all four.

## 5. The 40-add stress batch

Back-to-back (no sleep) from **node-1**, inside a **single SSH session** — one `ssh` per add
measures SSH+sudo+CLI startup, not PulseHA (the trap that produced the bogus 0.07s/IP figure).

Pick 40 addresses provably free in `10.200.0.0/23`. `arp -an` on a workstation that watched an
earlier run is a **false positive**; the nodes drop ICMP. The test that works is `arping -D`
**with a positive control**.

**Budget ~13s per add (defect #37) → ~9 minutes.** That is expected, not a hang.

Capture stdout, stderr and the exit code of every call.

## 6. Score it

```bash
# on each of the four nodes
sudo jq '.floating_ip_groups.RealTest | length' /etc/pulseha/config.json
```

- **PASS:** all four read `N0 + 40`, and every one of the 40 addresses is present on all four.
- **FAIL:** any node short. Compare against the CLI exit codes — an add that returned non-zero was
  never claimed to have succeeded and is not a #38 instance.

Wait out at least two coordinator reconcile firings (~2 min) and re-check: #38's mechanism was the
reconcile, so a count that is correct immediately and wrong two minutes later is the defect.

## 7. Logs to correlate

On node-1:
- `CONFIG_BROADCAST: peers did not accept the config after all retries` — run 19 had 17, 16 naming
  node-2. Expect these to still occur; they are the *trigger*, not the defect.
- **New in `e2c7143`:** `peer holds a newer config; this node's change will not propagate` — a Warn
  that fires when this node is the one behind. Should be absent unless a node was restarted
  mid-batch.

On node-2 (coordinator):
- `CONFIG_RECONCILE: re-broadcasting config from the coordinator` — once a minute, Debug.
- `CONFIG_SYNC: ignoring superseded config` with `version` and `held` — **this is the fix working.**
  Expect these on node-1 and node-3/4 when node-2's stale reconcile lands.

## If it passes

40 adds at a 4.5% base rate is a weak sample. Strengthen it either by:
- running 200 adds as run 19 did (~43 min at #37's cost), or
- widening the window deliberately: pause node-2's daemon briefly mid-batch so it *must* fall
  behind, then resume and let its reconcile fire. Pre-fix that erases everything node-2 missed;
  post-fix its reconcile is rejected as superseded.

The second is the sharper test and much faster.

## Leave-behind

Record in `docs/TEST-PLAN.md` (§5 row for #38 + a dated run-20 narrative) and update the
`pulseha-open-defects` memory. Note the final group size, mode, and whether debug was left on.
