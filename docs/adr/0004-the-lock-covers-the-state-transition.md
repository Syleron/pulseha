# The lock covers the state transition, never the network I/O

A member's lock is held long enough to change what the cluster believes, and released before
anything touches the network. `MakeActive` commits the new status and assignment set, unlocks,
and only then brings the addresses up. `EnterMaintenance` snapshots what it must release,
unlocks, brings those addresses down, and relocks to finish. Neither operation is atomic
end-to-end, and that is the decision — not an oversight to tidy up.

The consequence a reader meets first is that **an observer can see a member reported Active with
addresses that are not up yet.** That looks like a bug. It is the price, and it is paid
deliberately.

**Status: accepted, and only partly true of the code.** `MakeActive` and the repaired
`EnterMaintenance` follow the rule today, and `#4`/`#8` are the record of what it cost to learn
it. What does not follow it yet: `internal/server` still locks a `Member` and edits its exported
fields directly at 22 sites, the *claim* vocabulary the rule implies does not exist, and
`MakeActive`/`AddActiveIPs` still fuse the claim change to the bring-up rather than offering
either half. This document is the target those are being moved towards, not a description of
where they are. `END-2339`.

## Why the obvious alternative fails

Holding the lock across the bring-up is the natural thing to write, and it was what the code
did. Bringing up a large Floating IP Group touches the network once per address, and every
reader of that member's status needs the same lock — including the health-check responses its
peers use to decide it is still alive.

So an Active node with a big group **stopped answering health checks while it was doing the work
that made it Active**, and its peers concluded it was gone and elected around it. That is
`#4`/`#8`, and it is the failure this rule exists to prevent. The lock was not protecting the
member; it was making the member look dead.

`MakeActive`'s own comment states the trade honestly: a concurrent reader sees this node as
Active with its addresses assigned while they are still coming up, "which is the honest answer:
it owns them." The claim is a statement of intent and responsibility, not a report on the
kernel's routing table.

## The half that looks like paranoia

If the lock cannot span the I/O, then **anything needed across it must be snapshotted, and
re-validated afterwards.** That is the shape `EnterMaintenance` has:

1. Lock; snapshot the addresses to release; unlock.
2. Bring them down.
3. Lock; **re-check that the status has not moved**; clear the claim; unlock.

Step 3's re-check is the part that reads as defensive. It is not. The status genuinely can move
while the addresses are coming down — an election, a mode switch, a `ConfigSync` from a peer at
a higher epoch — and a node that is no longer Active must not have its assignment list cleared
out from under whatever moved it. Without this document that re-check looks removable, and
removing it loses a node's addresses.

The snapshot in step 1 is equally load-bearing and equally easy to "simplify": the field it
copies is cleared in step 3, so passing it by reference would hand the bring-down a slice that
the same function empties underneath it.

## What follows from the rule

- **`MakeActive` and `AddActiveIPs` each do two things**, and the split is along this line: a
  claim change, then a bring-up. Callers that want only the claim — a mode switch that
  deliberately defers the address work until the server lock drops — need the first half
  without the second, which is why the halves are separately callable rather than one method.
- **`RemoveActiveIPs` brings nothing down.** It is bookkeeping, called by the release pass which
  has already done the network work. That asymmetry is the rule, not an inconsistency.
- **`Member` owns its own fields.** `internal/server` cannot lock a member and edit it, because
  a caller holding the lock across arbitrary work is exactly what this rule forbids; it calls
  named operations instead. The one caller that genuinely needs read-modify-write atomicity —
  `ConfigSync`, applying a peer's view — passes a function that runs under the lock and can see
  and return only a claim. That is safe to offer *because* [ADR-0003](./0003-instrumented-mutexes.md)'s
  detector is in place: a closure that reaches back into a locking `Member` method panics in the
  suite and announces itself live, where before it would have wedged the member lock forever.
  This ordering is not incidental — the detector had to ship first.

## Consequences

- **Statuses and address reality are eventually consistent, by design.** A status read during a
  transition may not match the interfaces. Anything that needs the truth about an interface must
  ask the kernel: `#45`'s `AddrAddSatisfied` and `#33`'s per-address recheck immediately before
  its own `arping` both exist because a list built during a placement loop is stale by the time
  the work reaches its end.
- **There is a window, and it cannot be closed — only narrowed to the syscall.** Several writers
  add and remove addresses here: the enforce loop, the netlink watcher's restore, `BringUpIP`'s
  per-interface goroutines. A pre-check followed by an action is always a race; the rule makes
  it a short one instead of a lock that starves the health checks.
- **Announcements are always batched off a loop, never issued from one.** `restoreIP` runs on
  the netlink watcher's goroutine, whose channel has no overflow handling, and an `arping` costs
  ~4s — so announcing there would drop address events during exactly the churn that produces
  them. An earlier attempt routed it through the placement path and was reverted: the
  announcement was correct and the goroutine it would have run on was not.
