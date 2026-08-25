#!/usr/bin/env bash
#
# END-2320 -- a two-node cluster through a cluster-link-only partition and back.
#
# Two claims are under test, and they are not the same claim.
#
#   1. PARTITION. Both nodes bring up the floating IPs. This is the documented,
#      deliberate behaviour of a cluster with no majority: holding an address twice
#      is the only way to guarantee it is held at all. See
#      docs/adr/0002-two-node-availability-over-safety.md. Expected to pass before
#      any fix -- if it does not, the ADR is wrong, not the code.
#
#   2. HEAL. When the link returns, consolidation demotes one node. The survivor
#      held its addresses throughout, so no bring-up fires on it and -- before the
#      fix -- it announces nothing, while the node that just dropped them is the one
#      the segment learned from. The assertion is at the announcement boundary: the
#      survivor must emit an arping for each address it retains, AFTER the demotion.
#
# The mechanism assertion is the gate. The probe's reachability gap is the
# outcome-level control: it exists so that a fix which announces correctly but still
# leaves the address unreachable cannot pass. It is NOT a pass criterion on its own,
# because a Linux bridge relearns a MAC from the first frame the new owner sends and
# Linux re-ARPs aggressively on failure -- so this rig systematically UNDERSTATES the
# gap a real switch would show. Do not read its number as the customer impact.
#
# Usage:  ./test-2node-partition.sh [--keep]
#           --keep   leave the cluster running afterwards for inspection

set -uo pipefail

COMPOSE_FILE="docker-compose-2node.yml"
N1=pulseha-2node-1
N2=pulseha-2node-2
PROBE=pulseha-2node-probe

CLUSTER_IP1=10.66.0.10
CLUSTER_IP2=10.66.0.11
PORT=8080

FIP1=10.77.0.100
FIP2=10.77.0.101
GROUP=fipgroup

ARPING_LOG=/var/log/pulseha/arping.log
PROBE_LOG=/var/log/pulseha/probe.log

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

FAILURES=0
pass() { printf '  \033[32mPASS\033[0m  %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; FAILURES=$((FAILURES + 1)); }
info() { printf '        %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

cd "$(dirname "$0")" || exit 1

cleanup() {
    if [ "$KEEP" = "1" ]; then
        echo ""
        echo "Cluster left running. Tear down with:"
        echo "  docker compose -f docker/test/$COMPOSE_FILE down -v"
        return
    fi
    docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1
}
trap cleanup EXIT

# ---------------------------------------------------------------- helpers

# 45s, not 10s. `cluster join` exchanges certificates and routinely takes longer
# than ten seconds on a cold container; the shorter wrapper killed it mid-handshake
# and reported a join failure for a join that was working.
ctl() { local c=$1; shift; docker exec "$c" timeout 45s /usr/local/bin/pulsectl "$@" 2>/dev/null; }

# Interface holding a given address. Docker does not guarantee which of the two
# networks becomes eth0, so every interface name here is discovered, never assumed.
iface_for() {
    docker exec "$1" ip -4 -o addr show 2>/dev/null \
        | awk -v ip="$2" '$4 ~ "^"ip"/" {print $2; exit}'
}

holds_ip() {
    docker exec "$1" ip -4 -o addr show 2>/dev/null | grep -qE "inet ${2}/"
}

# node_id for a hostname, out of `pulsectl status --json`.
node_id_for() {
    ctl "$N1" status --json | python3 -c "
import json,sys
try: d = json.load(sys.stdin)
except Exception: sys.exit(1)
for m in d.get('members', []):
    if m.get('hostname') == sys.argv[1]:
        print(m.get('node_id','')); break
" "$1" 2>/dev/null
}

# Hostname of every member currently reporting Active.
#
# `pulsectl status --json` marshals the CLI's own struct, not the proto message, so
# `status` is the STRING "Active"/"Passive"/"Maintenance" -- not the MemberStatusEnum
# ordinal. Matching on 1 here silently reported "no Actives" on a healthy cluster.
actives_on() {
    ctl "$1" status --json | python3 -c "
import json,sys
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
for m in d.get('members', []):
    if str(m.get('status','')) == 'Active': print(m.get('hostname',''))
" 2>/dev/null
}

status_of() {
    ctl "$N1" status --json | python3 -c "
import json,sys
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
for m in d.get('members', []):
    if m.get('hostname') == sys.argv[1]: print(m.get('status','')); break
" "$1" 2>/dev/null
}

arping_lines() { docker exec "$1" sh -c "[ -f $ARPING_LOG ] && wc -l < $ARPING_LOG || echo 0" 2>/dev/null | tr -d ' \r'; }

# Announcements for $3 recorded on $1 after line offset $2.
arping_count_since() {
    docker exec "$1" sh -c "[ -f $ARPING_LOG ] || exit 0; tail -n +$(( $2 + 1 )) $ARPING_LOG | awk '\$3 == \"$3\"' | wc -l" 2>/dev/null | tr -d ' \r'
}

wait_for() { # wait_for <seconds> <command...>
    local deadline=$(( SECONDS + $1 )); shift
    while [ $SECONDS -lt $deadline ]; do
        "$@" >/dev/null 2>&1 && return 0
        sleep 1
    done
    return 1
}

# ---------------------------------------------------------------- bring up

step "1. Building and starting the two-node cluster"
docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1
if ! docker compose -f "$COMPOSE_FILE" up -d --build >/dev/null 2>&1; then
    echo "docker compose up failed"; docker compose -f "$COMPOSE_FILE" logs --tail 40; exit 1
fi

if ! wait_for 90 docker exec "$N1" /usr/local/bin/pulsectl status; then
    fail "node1 daemon never became ready"; docker logs "$N1" --tail 40; exit 1
fi
info "daemons up"

# The announcer must actually exist, or every assertion below reads zero for the
# wrong reason. This rig's whole premise is counting announcements.
if ! docker exec "$N1" sh -c 'command -v arping >/dev/null'; then
    fail "arping is not installed in the image -- announcements cannot be counted"; exit 1
fi
pass "announcer present on PATH"

step "2. Forming the cluster"
ctl "$N1" cluster create --bind-ip "$CLUSTER_IP1" --bind-port "$PORT" >/dev/null || { fail "cluster create failed"; exit 1; }
wait_for 60 docker exec "$N1" sh -lc "netstat -tln | grep -q '$CLUSTER_IP1:$PORT'" || { fail "node1 never listened"; exit 1; }

TOKEN=$(ctl "$N1" cluster token | head -1 | tr -d '\r\n' | xargs)
[ -z "$TOKEN" ] && { fail "could not read cluster token"; exit 1; }

JOINED=0
for attempt in 1 2 3; do
    if ctl "$N2" cluster join --address "$CLUSTER_IP1:$PORT" --token "$TOKEN" \
        --bind-ip "$CLUSTER_IP2" --bind-port "$PORT" >/dev/null 2>&1; then
        JOINED=1; break
    fi
    info "join attempt $attempt failed, retrying"
    sleep 5
done
[ "$JOINED" = "1" ] || { fail "node2 never joined the cluster"; docker logs "$N2" --tail 30; exit 1; }
sleep 10
info "cluster formed: $(actives_on "$N1" | tr '\n' ' ')"

step "3. Configuring floating IPs on the service network"
SVC_IFACE=$(iface_for "$N1" 10.77.0.10)
[ -z "$SVC_IFACE" ] && { fail "could not find node1's service interface"; exit 1; }
info "service interface on node1: $SVC_IFACE"

SVC_IFACE2=$(iface_for "$N2" 10.77.0.11)
[ -z "$SVC_IFACE2" ] && { fail "could not find node2's service interface"; exit 1; }

NODE1_ID=$(node_id_for node1)
NODE2_ID=$(node_id_for node2)
[ -z "$NODE1_ID" ] || [ -z "$NODE2_ID" ] && { fail "could not resolve node UUIDs"; exit 1; }

# InitiateJoin parks every joining node in Maintenance (`server.go`, the join handler
# sets `Maintenance: true`), and selectBestCandidate skips Maintenance -- so without
# this node2 can never be promoted and the partition half of the test measures a
# cluster with no failover at all rather than the behaviour under test.
ctl "$N2" node maintenance --disable >/dev/null 2>&1
for _ in $(seq 1 30); do
    [ "$(status_of node2)" = "Passive" ] && break
    sleep 1
done
info "node2 status after leaving maintenance: $(status_of node2)"

ctl "$N1" group create "$GROUP" >/dev/null
ctl "$N1" group add-ip --group "$GROUP" --ip "$FIP1/24" >/dev/null
ctl "$N1" group add-ip --group "$GROUP" --ip "$FIP2/24" >/dev/null
# Assigned to BOTH nodes' service interfaces. expectedIfaceIPs derives what a node
# should hold from that node's own IPGroups entry, so a node with no assignment
# expects nothing even while Active -- it would be promoted and bring up no address.
ctl "$N1" group assign --group "$GROUP" --node-id "$NODE1_ID" --interface "$SVC_IFACE" >/dev/null
ctl "$N1" group assign --group "$GROUP" --node-id "$NODE2_ID" --interface "$SVC_IFACE2" >/dev/null
info "assigned $GROUP to node1:$SVC_IFACE and node2:$SVC_IFACE2"

if wait_for 60 holds_ip "$N1" "$FIP1"; then
    pass "node1 is serving the floating IPs"
else
    fail "node1 never brought the floating IPs up"; docker logs "$N1" --tail 40; exit 1
fi

# ---------------------------------------------------------------- partition

step "4. Severing the cluster link (service network left intact)"
docker exec "$PROBE" sh -c "rm -f $PROBE_LOG; nohup sh -c 'while true; do \
    if ping -c1 -W1 $FIP1 >/dev/null 2>&1; then echo \"\$(date +%s) up\"; else echo \"\$(date +%s) DOWN\"; fi; \
    sleep 1; done' >> $PROBE_LOG 2>&1 &"
sleep 2

for spec in "$N1 $CLUSTER_IP2" "$N2 $CLUSTER_IP1"; do
    set -- $spec
    docker exec "$1" iptables -A INPUT  -s "$2" -j DROP
    docker exec "$1" iptables -A OUTPUT -d "$2" -j DROP
done
info "cluster link down; both nodes still on the service network"

# Wait for the failover to CONVERGE rather than sampling at a guessed instant. A
# fixed 45s sample caught node2 mid-election and reported "only one node holds the
# group", which is the opposite of what the cluster was doing four seconds later.
#
# The condition waited on is node2 promoting -- the change this partition is
# supposed to cause -- not "both nodes hold", which would be waiting for the answer
# the test is meant to measure. If it never happens the deadline expires and the
# assertions below report what was actually there.
PARTITION_WAIT=150
promoted_at=""
deadline=$(( SECONDS + PARTITION_WAIT ))
start=$SECONDS
while [ $SECONDS -lt $deadline ]; do
    if holds_ip "$N2" "$FIP1"; then promoted_at=$(( SECONDS - start )); break; fi
    sleep 2
done
if [ -n "$promoted_at" ]; then
    info "node2 claimed the group ${promoted_at}s after the link went down"
else
    info "node2 had not claimed the group after ${PARTITION_WAIT}s"
fi
# Let the slower side settle before sampling both.
sleep 10

ACTIVES_1=$(actives_on "$N1" | tr '\n' ' ')
ACTIVES_2=$(actives_on "$N2" | tr '\n' ' ')
info "node1's view of Actives: ${ACTIVES_1:-none}"
info "node2's view of Actives: ${ACTIVES_2:-none}"

N1_HOLDS=0; N2_HOLDS=0
holds_ip "$N1" "$FIP1" && N1_HOLDS=1
holds_ip "$N2" "$FIP1" && N2_HOLDS=1
info "ip addr: node1 holds=$N1_HOLDS  node2 holds=$N2_HOLDS"

# Claim 1, straight from the ticket. Deliberate behaviour, not a defect.
if [ "$N1_HOLDS" = "1" ] && [ "$N2_HOLDS" = "1" ]; then
    pass "both nodes brought up the floating IPs (ADR-0002: the group is never dark)"
elif [ $(( N1_HOLDS + N2_HOLDS )) = "1" ]; then
    fail "only one node holds the group -- ADR-0002 says a partitioned pair serves from both"
else
    fail "NEITHER node holds the group -- the address is dark, which ADR-0002 forbids outright"
fi

# ---------------------------------------------------------------- heal

step "5. Restoring the cluster link"
MARK_1=$(arping_lines "$N1")
MARK_2=$(arping_lines "$N2")
info "announcement log offsets at heal: node1=$MARK_1 node2=$MARK_2"

for c in "$N1" "$N2"; do docker exec "$c" iptables -F; done
info "link restored; waiting for consolidation"

# Same reasoning as the partition wait: converge, do not guess. Consolidation is
# coordinator-gated and runs a MakePassive that can itself take time, so the
# condition is "exactly one node still holds the group".
HEAL_WAIT=180
consolidated_at=""
deadline=$(( SECONDS + HEAL_WAIT ))
start=$SECONDS
while [ $SECONDS -lt $deadline ]; do
    h1=0; h2=0
    holds_ip "$N1" "$FIP1" && h1=1
    holds_ip "$N2" "$FIP1" && h2=1
    if [ $(( h1 + h2 )) = "1" ]; then consolidated_at=$(( SECONDS - start )); break; fi
    sleep 3
done
if [ -n "$consolidated_at" ]; then
    info "consolidated to a single holder ${consolidated_at}s after the link came back"
else
    info "still not consolidated after ${HEAL_WAIT}s"
fi
sleep 10

ACTIVES=$(actives_on "$N1" | tr '\n' ' ')
info "Actives after heal: ${ACTIVES:-none}"

N1_HOLDS=0; N2_HOLDS=0
holds_ip "$N1" "$FIP1" && N1_HOLDS=1
holds_ip "$N2" "$FIP1" && N2_HOLDS=1
info "ip addr: node1 holds=$N1_HOLDS  node2 holds=$N2_HOLDS"

if [ $(( N1_HOLDS + N2_HOLDS )) = "1" ]; then
    pass "the cluster consolidated onto exactly one node"
else
    fail "expected exactly one holder after heal, got node1=$N1_HOLDS node2=$N2_HOLDS"
fi

if [ "$N1_HOLDS" = "1" ]; then SURVIVOR=$N1; MARK=$MARK_1; SNAME=node1
else SURVIVOR=$N2; MARK=$MARK_2; SNAME=node2; fi

# ------------------------------------------------- the mechanism assertion

step "6. Did the survivor announce what it kept?"
A1=$(arping_count_since "$SURVIVOR" "$MARK" "$FIP1")
A2=$(arping_count_since "$SURVIVOR" "$MARK" "$FIP2")
info "announcements by $SNAME since heal: $FIP1 x$A1, $FIP2 x$A2"

# Nothing moved onto the survivor, so before the fix no bring-up fires on it and
# these are both zero -- while the demoted node, whose bring-up announced last, has
# just dropped the addresses the segment learned from it.
if [ "${A1:-0}" -ge 1 ] && [ "${A2:-0}" -ge 1 ]; then
    pass "the survivor re-announced every retained address after consolidation"
else
    fail "the survivor announced nothing for at least one retained address"
    info "this is the dark window: the segment still points at the demoted node"
fi

# ------------------------------------------------- the outcome control

step "7. Reachability across the whole cycle (control, not a gate)"
docker exec "$PROBE" sh -c "pkill -f 'ping -c1' >/dev/null 2>&1; pkill -f 'while true' >/dev/null 2>&1" || true
TOTAL=$(docker exec "$PROBE" sh -c "wc -l < $PROBE_LOG 2>/dev/null || echo 0" | tr -d ' \r')
# grep -c prints 0 AND exits non-zero when there are no matches, so the `|| echo 0`
# fired as well and the two zeros concatenated into "0\n0".
DOWN=$(docker exec "$PROBE" sh -c "grep -c DOWN $PROBE_LOG 2>/dev/null; true" | head -1 | tr -d ' \r')
DOWN=${DOWN:-0}
info "probe samples: $TOTAL, unreachable: $DOWN"
if [ "${DOWN:-0}" = "0" ]; then
    info "no gap observed -- expected on a Linux bridge, which is why this is not a gate"
else
    info "gap observed across ${DOWN}s of samples; a real switch would show a longer one"
fi

# ---------------------------------------------------------------- verdict

step "Result"
if [ "$FAILURES" = "0" ]; then
    printf '  \033[32mAll assertions passed\033[0m\n'
else
    printf '  \033[31m%d assertion(s) failed\033[0m\n' "$FAILURES"
fi
echo ""
echo "Limitation: a Linux bridge is not a switch. This rig proves whether the"
echo "announcement was sent, not how long the address would stay dark without it."
exit "$FAILURES"
