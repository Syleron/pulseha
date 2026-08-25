#!/usr/bin/env bash
#
# Runs TC-9 against a binary with the consolidation re-announcement REMOVED, then
# restores the tree. This is the counterfactual for defect #80: without it, a green
# TC-9 proves only that the rig passes, not that the fix is what makes it pass.
#
# The file is backed up, patched, and restored from a trap on EXIT/INT/TERM, so an
# interrupted run cannot leave the fix silently disabled in the working tree.
#
# Usage:  ./counterfactual-run.sh   (from the repo root or anywhere)

set -uo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
TARGET="$REPO/internal/membership/health_check.go"
BACKUP="$(mktemp)"
LOG="${1:-/tmp/2node-counterfactual.log}"

restore() {
    if [ -s "$BACKUP" ]; then
        cp "$BACKUP" "$TARGET"
        echo "restored $TARGET"
    fi
    rm -f "$BACKUP"
}
trap restore EXIT INT TERM

cp "$TARGET" "$BACKUP"

# Disables the health-check demotion detector.
#
# Not enforceSingleActive's announce, and not a ConfigSync hook: both were tried and
# neither is the path a two-node heal takes. Consolidation never fires (the member
# states converge through ConfigSync first) and a receive-side hook only fires on the
# node that is told of the demotion, which node-ID ordering makes the survivor about
# half the time. The detector on the health-check pass is the one that covers both.
python3 - "$TARGET" <<'PYINNER'
import io, sys
p = sys.argv[1]
s = io.open(p, encoding='utf-8').read()
needle = u"""	h.announceOnPeerDemotion(members)"""
if needle not in s:
    sys.exit("could not find the demotion detector call to disable")
s = s.replace(needle, u"""	_ = h.announceOnPeerDemotion // disabled for the counterfactual""", 1)
io.open(p, 'w', encoding='utf-8').write(s)
print("demotion detector disabled")
PYINNER
[ $? -eq 0 ] || exit 1

if ! (cd "$REPO" && go build -mod=mod ./... ); then
    echo "mutated tree does not build"; exit 1
fi

echo "running TC-9 against the unfixed binary; log: $LOG"
"$REPO/docker/test/test-2node-partition.sh" > "$LOG" 2>&1
rc=$?

echo ""
echo "=== counterfactual result (expected: the announce assertion FAILS) ==="
grep -E "PASS|FAIL|announcements by" "$LOG" || true
echo ""
echo "exit code: $rc"
