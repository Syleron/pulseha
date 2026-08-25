#!/bin/sh
# Records every announcement PulseHA makes, then performs it.
#
# Installed at /usr/local/bin/arping, ahead of the real binary on PATH, because
# packages/network requireAnnouncer resolves the announcer by name through PATH.
# One line per invocation, appended atomically enough for a counter:
#
#   <unix-seconds> <iface> <ip> <argv...>
#
# The interface and address are pulled out of the argv PulseHA builds --
# `arping -U -c 5 -I <iface> <ip>` -- rather than assumed positionally, so a
# change to the flags shows up as an empty field instead of a wrong one.
#
# Exit status is the real binary's, untouched. PulseHA reads it: an address the
# interface does not hold exits 2, and SendGARPBatch reports those as skipped
# (defect #33). Swallowing or rewriting it here would silently repair a failure
# the daemon is supposed to notice.

LOG="${ARPING_LOG:-/var/log/pulseha/arping.log}"

iface=""
ip=""
prev=""
for arg in "$@"; do
    case "$prev" in
        -I) iface="$arg" ;;
    esac
    case "$arg" in
        -*) ;;
        *) [ "$prev" != "-I" ] && [ "$prev" != "-c" ] && ip="$arg" ;;
    esac
    prev="$arg"
done

mkdir -p "$(dirname "$LOG")" 2>/dev/null
printf '%s %s %s %s\n' "$(date +%s)" "${iface:--}" "${ip:--}" "$*" >>"$LOG" 2>/dev/null

for real in /usr/sbin/arping /sbin/arping /bin/arping /usr/bin/arping; do
    [ -x "$real" ] && exec "$real" "$@"
done

echo "arping-shim: no real arping binary found" >&2
exit 127
