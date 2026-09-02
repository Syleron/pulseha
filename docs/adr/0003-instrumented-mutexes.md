# Mutexes are instrumented, and reentrancy is fatal only under test

Every mutex in the daemon is a `pulselock.Mutex` or `pulselock.RWMutex` rather than the
standard library's. They embed and wrap `sync.Mutex`/`sync.RWMutex` and match their method
sets exactly, so `s.Lock()` still means what it always meant — but a lock the daemon has wedged
itself on now says so instead of hanging in silence. A reader who finds this wrapper and reads
it as gold-plating will replace it with `sync.RWMutex`, which is why it is written down.

**Status: accepted, and being applied.** `packages/pulselock` exists and is tested; the 18
declarations across `internal/server`, `internal/membership`, `internal/quorum` and
`packages/config` are being converted, and a test forbidding bare `sync` mutexes lands with
them. Until that is finished this document describes the decision rather than the whole tree —
`grep -rn 'sync\.\(RW\)\?Mutex' internal/ packages/` is the honest measure of how far it has
got. `END-2339`.

## What it is for

Go's mutexes are not reentrant, so a method holding a lock cannot call a sibling that takes the
same lock. Nothing enforced that, and it produced **nine** deadlocks: `#32`, `#46`, `#56`,
`#85`, `#87`, plus `RebalanceCluster`, `hasQuorumLocked`, `Server.PromoteNode` and
`Member.RemoveIPs`.

The number is not the argument, though. The argument is that **every one of the nine was a
method calling a locking sibling on its own receiver, inside its own file.** Not one was an
external caller taking the lock. So the constraint was already local, already in one place, and
already reviewable — and it was still missed nine times, by people who knew about it. That is
the observation the whole decision rests on: this is not a problem of visibility.

What makes them worth catching is the failure mode. `#56` wedged the health-check goroutine
*while it held the write lock*: no node was promoted, a 287-address group stayed down, and
`ACTIVE_CHECK: Starting active node failure check` appeared **zero times in six minutes**. A
deadlock here produces no error, no log line and no crash — just a daemon that has stopped
having opinions. Three of the nine were found on live clusters, and one (`Member.RemoveIPs`) was
found only by reading, having sat in the tree unexecuted.

## Why not un-embed the mutex

The obvious fix, and the one originally proposed, is to make the mutex a named private field —
`mu sync.RWMutex` — so `Lock()` leaves each type's public surface and all locking is internal.

It addresses none of the nine. `s.Lock(); defer s.Unlock(); … s.GetClusterEpoch()` deadlocks
identically after the rename; it just spells the first call `s.mu.Lock()`. The locking was
*already* internal in all nine cases, which is the observation above.

It also makes `internal/server` worse. `Server` has **nine** mutexes: the main lock plus
`peerBringUpMu`, `vipReconcileMu`, `clientMutex`, `reconfigureMu`, `clusterInitMu`,
`propagationMu`, `clusterListenMu` and `announceMu`. Today `s.Lock()` unambiguously means the
big one and `s.peerBringUpMu.Lock()` means a small one. Rename it to `s.mu` and it becomes one
entry in a list of nine, with nothing marking which carries the discipline problem.

`Member` is un-embedded anyway, for a different and real reason — it is the one type handed
across a package boundary, and `internal/server` was reading and writing its exported fields
bare. That is a data race rather than a deadlock, and belongs to [ADR-0004](./0004-the-lock-covers-the-state-transition.md)'s
rule, not to this one.

## Why not a static check

Also proposed: ship a static pass that fails on a locking method calling a locking sibling
while holding the lock, with an allowlist for the deliberate ones.

It was tried. Over `internal/membership` and `internal/server` it produced **10 candidates: 9
false positives, and the 1 true positive was dead code** (`Server.PromoteNode`, deleted rather
than repaired). It also **missed `Member.RemoveIPs → BringDownIPs`, which was live** — a
`m.Lock()` with a deferred unlock calling `BringDownIPs`, which takes the same lock, on both
branches.

The false positives say what a sound version would cost. Seven of the nine were a callee taking
a *different* mutex on the same receiver — `getPeerClient → s.clientMutex`, `peerBringUpQueue →
s.peerBringUpMu`, `memberStatesForBroadcast → a Member's lock`. Those eight auxiliary mutexes
exist **precisely so** they can be taken under the main lock. So the check must resolve *which*
mutex, transitively, through the call graph. That is a real analysis, and it would be answering
statically a question the runtime can answer exactly.

What is worth checking statically is something else, and is checked: that no bare `sync.Mutex`
or `sync.RWMutex` is declared in daemon code. That is lexically decidable, has no false
positives and needs no allowlist.

## Why the exact mechanism does not run in production

Detecting reentrancy exactly requires knowing which goroutine holds the lock, and Go exposes no
cheap way to ask. `runtime.Stack`'s header is the only route, and it costs **~1710ns against
~16.5ns** for the uncontended lock it would guard — around **100x**. It cannot sit on the
health-check path.

So there are two mechanisms:

- **Under `go test`**, goroutine identity is tracked and a reentrant acquisition **panics
  immediately**, naming what was already held. Exact, and the cost is irrelevant.
- **In the daemon**, an acquisition still blocked after `pulselock.ReportAfter` (30s) writes
  every goroutine's stack to stderr and **keeps waiting**.

The production half is a *wedge* detector, not a reentrancy detector. It is less precise and
catches strictly more: all nine historical shapes block forever, and so do lock-ordering cycles
between two objects, which no reentrancy check can see.

## Why it logs and blocks rather than panicking

A reentrant acquisition is a guaranteed deadlock, so panicking loses that goroutine nothing —
and `Restart=on-failure` with `RestartSec=10` would bring the daemon back. It still isn't the
right default. There is no `recover()` anywhere in this codebase, so a panic is a hard process
exit; on the two-node configuration [ADR-0002](./0002-two-node-availability-over-safety.md)
already calls delicate, a 10-second daemon gap is a failover event, and the wedge may be on a
path (`pulsectl node maintenance`) that is not otherwise serving traffic.

"Log and carry on" is not available: the next statement *is* the real acquisition, and skipping
it would run the critical section unguarded — trading a deadlock for a data race. So the
production behaviour is deliberately **identical to an uninstrumented mutex**. The daemon wedges
exactly as it does today. It just says so first, which is the whole change.

## The diagnostic goes to stderr, not the logger

`#33`/`#61` record a Debug line in `packages/network` that **could not reach the journal at any
`logging_level`**, because nothing ever calls `SetLevel` on that package's logger — which made
the fix it was meant to evidence unverifiable live. `pulselock` is a leaf package and would have
had exactly that problem. Under `Type=simple` with no `StandardOutput` override, stderr reaches
`journalctl` unconditionally.

This is also why the ticket's live check is to *inject* a reentrant acquisition on a running
daemon and confirm the line appears. A detector for silent failures whose own output is silent
is `#61` again, and finding that out at the tenth deadlock would defeat the point.

## Consequences

- Locking costs a few nanoseconds more: ~+9% on an uncontended write lock (+1.5ns), parity
  within noise on a contended read, ~+10% on a realistic read-heavy mix. Committed as
  benchmarks in `packages/pulselock/pulselock_bench_test.go` rather than quoted, because the
  decision turns on them.
- A test that deadlocks now **panics with both acquisition sites** instead of timing out the
  package, which is a large difference in how long a defect takes to understand.
- Read-then-read on an `RWMutex` is reported but **not fatal**. It deadlocks only when a writer
  queues between the two acquisitions, so calling it a defect outright would be wrong.
- The wrapper says nothing about **unsynchronised** access. A field read without the lock is a
  data race, and `-race` is the tool for it — noting that `#71` records `-race` missing one
  *structurally*, because nothing in the suite drove the two paths concurrently.
