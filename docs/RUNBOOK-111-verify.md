# Runbook — verify #111 (cluster TLS) live on the MC-LB-3 pair

Verifies the `cluster-tls-required` branch: ADR-0005 steps 3b and 4. Written 2026-09-14.

> **EXECUTED 2026-09-14 — PASS.** See `TEST-PLAN.md` TC-3 for the evidence. Five things this
> runbook got wrong, kept here so the next run does not repeat them:
> 1. **There is no `tcpdump` on the appliance.** Use `openssl s_client -connect <ip>:9083`
>    instead, which is a sharper control anyway: before the flip it returns
>    `no peer certificate available` with no cipher, and after it completes a TLS 1.3 handshake
>    presenting exactly that node's own certificate, which a capture would not tell you.
>    `Verify return code: 21` is correct — there is no CA, which is the design.
> 2. **The certificates are at `/root/.pulseha/certs/`**, not `~/.pulseha/certs` for the SSH
>    user. The daemon runs as root; the config is at `/etc/pulseha/config.json`, which is a
>    different directory, and §3 said so but guessed the wrong home.
> 3. **Do not remove a node while the cluster is `required` and then flip back.** The removed
>    node never receives the flip back — it is no longer in the config — so it is stranded on
>    `required` and cannot even `cluster leave`, failing with
>    `tls: first record does not look like a TLS handshake`. §9's console recovery fixes it and
>    was exercised doing exactly this. Do §8 last, or re-join before flipping back.
> 4. **A join lands the node in maintenance.** Finish with `pulsectl node maintenance --disable`
>    or the cluster reads `degraded` and nothing is wrong.
> 5. Each join mints a **new node UUID**, so `--node-id` values from before a re-join are stale.

**Read this first.** Everything in #111 above step 3a is built and tested and **none of it is
verified live**. Three of the defects in `#104`-`#110` were invisible to the test suite, and the
two things this branch does — putting credentials on a listener, and taking a running cluster
from plaintext to TLS without breaking it — are precisely the two things a single appliance
cannot demonstrate. A green suite is the entry condition for this runbook, not a substitute for
it.

**This runbook can take the cluster off the air.** §7 is a deliberate severing test, and §4 is a
change that, if it goes wrong, leaves a node unreachable over the network. Read §9 (recovery)
before starting, and have console access to both nodes.

## 0. Preconditions

- Two nodes, `MC-LB-3-node-1` and `MC-LB-3-node-2`, cluster port **9083** (not 8080 — the
  nftables ruleset permits 9083 with `comment "ID-pulseha"` and nothing on 8080, which is #114).
- Both nodes on the **same build** from this branch. A mixed pair is a separate test and is not
  this one.
- `logging_level = "debug"` on **both** nodes, set in each `config.json` and the daemon
  restarted — the level is applied at startup only (`cmd/pulseha/main.go:79`).
- Cluster healthy, both `Passive`/`Active` as expected, floating IPs settled. Record the
  settled distribution; §4 must not change it.

**Control first.** Before concluding anything from the absence of a log line, prove Debug is
reaching the journal: grep for `heartbeat convergence nudge`, which fires every three health
checks. If that is absent, Debug is off and no other absence means anything. This trap produced
a wrong conclusion in run 19.

## 1. Build and deploy

```bash
make build && make cli
md5sum cmd/pulseha/bin/pulseha            # record; verify against each node after deploy
```

Do **not** build with `-mod=mod` — it rewrites `go.mod`/`go.sum`.

Restart one node at a time, passive first. Never cold-start both (#23).

## 2. Baseline: the cluster is plaintext, and prove it

This is the "before" evidence, and it is worth capturing properly because it is the only chance
to show what the change removes.

```bash
# On node-1, while the cluster is idle-but-talking:
tcpdump -i any -s0 -A 'tcp port 9083' -c 200 -w /tmp/pre-tls.pcap
strings /tmp/pre-tls.pcap | grep -i -m5 'cluster_token\|tls_cert\|ConfigSync'
```

Expect readable gRPC frames, and the **cluster token in clear**. That is the defect.

On each node's journal, expect:

```
Serving cluster gRPC   addr=…:9083  tls=false
```

`tls=false` here is the positive control for the whole run: if this line is missing, the version
deployed is not this branch and nothing below means anything.

## 3. The trust set is complete before anything is flipped

```bash
pulsectl config get                        # the daemon's live config, not the file
jq '.nodes | map_values(.tls_cert != null)' /etc/pulseha/config.json
```

On **both** nodes, both node entries must carry a `tls_cert`, and the two nodes must agree about
both. Compare fingerprints across the pair:

```bash
# on each node. Note the two directories are NOT the same: the config lives in
# /etc/pulseha when the daemon runs as root, and the certificates live under
# $HOME/.pulseha/certs (packages/security.CertDir) — which for a systemd unit is
# root's home, not /etc/pulseha. Check where the daemon actually wrote them
# before assuming: `ls -l ~/.pulseha/certs/`.
openssl x509 -in ~/.pulseha/certs/pulseha.crt -outform DER | sha256sum   # this node's own
jq -r '.nodes[].tls_cert' /etc/pulseha/config.json | while read -r c; do
  printf '%b' "$c" | openssl x509 -outform DER 2>/dev/null | sha256sum
done
```

Each node's own certificate must appear in the other's config, byte for byte. This is step 2,
already verified live, and it is re-checked because the flip in §4 depends on it entirely.

## 4. The flip, which is the test

Run on **node-1**:

```bash
pulsectl cluster token                    # record: should still be the bare secret
pulsectl config set tls_mode required
```

Expected reply: `applied to every node in the cluster`. Anything naming a node
(`applied to every node in the cluster, except MC-LB-3-node-2 — that node did not accept the
change and will be unreachable until it does`) means the delivery failed and §9 applies
immediately, on node-2's console.

**Watch both journals through this.** In order:

| Node | Line | Meaning |
|---|---|---|
| node-1 | `Cluster TLS mode changed  tls_mode=required` | written locally |
| node-2 | `Serving cluster gRPC  addr=…:9083  tls=true` | node-2 took the change **and rebound** |
| node-1 | `Serving cluster gRPC  addr=…:9083  tls=true` | node-1 applied it to itself, **last** |

The order matters and is the thing most likely to be wrong. node-1 must not rebind before node-2
has been told: the change is delivered over the plaintext channel it removes, and a node-1 that
flipped first would have no way left to tell node-2. If node-2's line never appears, the delivery
path is broken — that is the defect the unit test caught once already, and it would mean the
in-clear push regressed.

Then, within `fo_limit` (10s by default), confirm **nothing failed over**:

- `pulsectl status` on both: same roles as §0, same floating-IP distribution.
- No `marked unreachable`, no promotion, no `redistribut` lines in either journal.
- **Watch for a full `fo_limit` and then some.** The failure this most resembles is
  slow: cached connections that survived the flip keep failing, each node stops
  hearing the other, and on a two-node cluster that is the tiebreak path. A clean
  first ten seconds is not a pass — leave it settling for several minutes and
  re-check `pulsectl status` on both.

A failover here is a **fail**, not a wobble: the whole precondition argument is that the window
between the push and the last node applying it is far inside the failover margin.

## 5. The wire is now encrypted

```bash
tcpdump -i any -s0 -A 'tcp port 9083' -c 200 -w /tmp/post-tls.pcap
strings /tmp/post-tls.pcap | grep -i 'cluster_token\|ConfigSync'     # expect NOTHING
```

Expect TLS records and no readable gRPC. Confirm the version negotiated is 1.3:

```bash
tshark -r /tmp/post-tls.pcap -Y 'tls.handshake.type == 1' -T fields -e tls.handshake.version
```

**Both halves are needed.** An empty grep alone proves nothing if the capture caught no traffic —
check the packet count is non-trivial and that `ConfigSync` was readable in §2's capture on the
same filter.

On node-1's journal at the moment of the flip, expect the cached connections to be
thrown away — without this the broadcasts keep using plaintext connections made
before the flip and nothing ever replaces them:

```
Dropped cached peer connections after a TLS mode change   pooled=1 tls=true
```

Also confirm peers are actually being dialled on the new terms:

```
Connected to peer   address=…:9083  tls=true
```

(Debug, on the daemon's logger. The `Client:Connect()` line in `internal/client` is on
charmbracelet's default logger, which nothing calls `SetLevel` on — it cannot reach the journal
at any level, and is not evidence of anything. That is #61's lesson and the reason the line above
exists.)

## 6. The preconditions actually refuse

With the cluster on `required`, flip it back and try to move it forward under conditions that
should be refused. **Each of these must fail, and the reply must name the node.**

```bash
# Back to the safe state first. This direction is delivered over TLS, because that
# is what the peers are still serving — watch node-2 for `Serving cluster gRPC …
# tls=false` to confirm it took the change. A node-2 that never reports it has not
# come back to plaintext, and §9 applies on its console.
pulsectl config set tls_mode permissive
systemctl stop pulseha                         # on node-2
# wait for node-1 to see it as unreachable
pulsectl config set tls_mode required          # on node-1 — MUST be refused
```

Expect: `every node must be reachable and have published a certificate before TLS can be required
(not reachable: MC-LB-3-node-2)`.

Then, with node-2 back up, blank its `tls_cert` in node-1's config by hand and retry — expect
`no published certificate: MC-LB-3-node-2`. Restore the certificate afterwards.

A refusal that does not name the node is a partial pass: the operator has to know where to go.

## 7. A node joins a cluster that already requires TLS

This is step 3b, and it is the part with no plausible unit-test substitute.

1. Take node-2 out: `pulsectl cluster leave` on node-2, confirm node-1 holds a one-node cluster.
2. Flip node-1 to `required` (it will now pass §4's preconditions on its own).
3. On node-1: `pulsectl cluster token`. It must now be **two parts**, `<uuid>.<64 hex>`, and the
   hex must equal node-1's own certificate fingerprint from §3. A bare token here is a fail —
   the joiner would have nothing to pin.
4. On node-2: `pulsectl cluster join --address <node-1 ip>:9083 --token '<the whole token>'`.

Expected on node-2: `INITIATE_JOIN: the token pins the cluster's certificate  fingerprint=…`.
Expected on node-1: `Accepting a join from a certificate the cluster does not name yet`, then
the ordinary `Authorised a peer against the cluster trust set  node=<node-2 uuid>` on its next
RPC. The second line is what proves the joiner moved from the one-door bootstrap into the trust
set; without it the join half-worked.

**Then the two negative cases, both of which must fail:**

- **A truncated token.** Pass the token with the final hex character removed. Expect
  `the join token does not carry a usable certificate fingerprint`. It must **not** fall back to
  a plaintext join — that is the silent downgrade the unit test found, and a live plaintext join
  here would be the same defect returning by another route. Check the capture: no plaintext gRPC
  on 9083 during the attempt.
- **A token naming the wrong certificate.** Replace the hex with node-2's own fingerprint. Expect
  the dial to fail at the handshake, naming both fingerprints.

## 8. Revocation is removal

With the pair back together on `required`:

```bash
pulsectl node remove --node-id <node-2 uuid>       # from node-1
```

Then attempt any RPC from node-2 to node-1. Expect on node-1:

```
Refused an RPC from a certificate the cluster does not name   method=/rpc.Server/HealthCheck
```

`PermissionDenied` on the caller. This must happen **without restarting node-1's listener** —
the trust set is read per call, and a node-1 that only refuses after a rebind means it was
captured at bind time, which is the mutation the unit tests exist to catch.

## 9. Recovery, and read this before §4

A node that is on a different `tls_mode` from its peers cannot talk to them **at all**, so a
botched flip is not repairable over the network. On the console of the affected node:

```bash
systemctl stop pulseha
# edit /etc/pulseha/config.json: set "pulseha": { … "tls_mode": "permissive" }
# (or delete the key — absent reads as permissive)
systemctl start pulseha
```

Do this on **every** node, then bring them up one at a time. A hand-edited `tls_mode` that is
neither name reads as permissive rather than refusing to start — that is deliberate, so a typo
during recovery leaves the cluster talking rather than dead.

Certificates are **not** the thing to delete during recovery, and the reason is sharper than
"it makes things worse". `EnsureCertificates` regenerates only what it cannot use, and a
regenerated certificate is a *new identity* that no peer's trust set names. On a cluster that
requires TLS that is asymmetric, and the asymmetry is the trap:

- peers can still reach the node — their certificates are in **its** trust set, so its
  interceptor authorises them;
- the node cannot reach them — it presents a certificate none of **their** trust sets name, so
  their interceptors refuse everything it sends **except `Join`**, including the `ConfigSync`
  that would publish its new certificate.

So it cannot announce itself out of the hole, and its own health checks of its peers fail while
theirs of it succeed. In active-passive that is the shape that ends with it deciding its peer is
gone and promoting itself — the duplicate-address outcome of defects #2/#26, reached from a new
direction.

The daemon reports this at startup, and it is worth grepping for after any restart of a node on
a `required` cluster:

```
This node's certificate is not the one the cluster knows it by, and the cluster requires TLS
```

**The way out is a local reset and then a re-join** — a re-join alone is not enough, and this was
measured rather than guessed. `pulsectl cluster leave` on the affected node fails with
`PermissionDenied … is not in the cluster trust set`, because leaving coordinates the removal with
the peers that are refusing it; and `cluster join` then refuses with `node is already part of a
cluster; leave first`. So, on the affected node's console:

```bash
sudo systemctl stop pulseha
# edit /etc/pulseha/config.json: keep only this node's own entry under "nodes",
# and set "tls_mode": "permissive" so the join itself can proceed
sudo systemctl start pulseha
sudo pulsectl cluster leave                    # now succeeds, local state only
sudo pulsectl cluster join --address <healthy node>:9083 --token '<full token>' --bind-ip <this node>
sudo pulsectl node maintenance --disable       # a join always lands in maintenance
```

Remove it from the healthy node first (`pulsectl node remove --node-id …`) so it is not carrying
a stale entry. `Join` is the one RPC an unnamed certificate may call, and the join records the
joiner's certificate, which is what puts it back in the trust set.

The node also takes itself **out of failover promotion** while this is true, so it cannot elect
itself against a healthy cluster it simply cannot reach. Expect, beside the line above:

```
Holding this node out of failover promotion until its certificate is back in the cluster's trust set
```

`pulsectl status` will show it as `Maintenance` and the cluster as `degraded`, which is the guard
working rather than a second fault. It releases itself after a successful re-join — no
`maintenance --disable` needed, and an operator's own maintenance is never cleared by it.

## 10. If it passes

Record in `TEST-PLAN.md` TC-3 with the evidence, not the conclusion: the two `Serving cluster
gRPC` lines with their `tls=` values and timestamps, the before/after `strings` output, the
join's two log lines, and the exact refusal messages from §6 and §8. A run that reports "TLS
works" without the lines is not a verified run.

**And state what it does not cover.** This is a two-node pair on one build. Not covered:

- **A mixed build during a rolling upgrade.** A node on an older binary has no `tls_mode` key,
  stays plaintext, and is severed by a flip — which is why the procedure is upgrade every binary
  first and flip second. It no longer *un-flips* the cluster, which it would have: such a binary
  deletes the key from every config it re-broadcasts, and an absent key is now read as "the sender
  has no opinion" rather than as permissive. That guard is tested and is itself unverified live.
  If you want to exercise it, deploy the old binary to node-2 after §4 and confirm node-1 stays on
  `required` across several of the coordinator's once-a-minute re-broadcasts, logging
  `incoming config does not mention tls_mode; keeping this node's`.
- **Clusters larger than two.** The flip's delivery loop is per peer and the window it opens
  scales with the slowest one.
- **Certificate expiry and rotation**, which ADR-0005 explicitly defers. A ten-year self-signed
  certificate postpones the question rather than answering it.
