# Verified IP transfers (END-2695)

This change is stacked on #263. It makes cooperative address transfers depend on
observed release and acquisition, rather than successful RPC transport or an
optimistic `Success` flag.

## Contract and ordering

`BringUpIP` and `BringDownIP` return verification version 1 and partition the
requested addresses into `verified_ips` and `failed_ips`. Each result comes from
a fresh post-operation interface inventory. Up means the address is present on
the requested interface; down means it is absent everywhere on that node. These
are address-presence checks, not prefix-length or traffic-delivery assertions.
An incomplete inventory fails verification. Bring-up also reports failed
placement or announcement work rather than claiming completion.

Before changing either node, orchestration validates every requested address and
its source/destination interface mapping. Missing or ambiguous mappings reject
the entire plan. For a named source, all release batches must confirm success
before any destination batch is requested. A destination failure returns an
error; confirmed portions are retained in ownership bookkeeping and unconfirmed
portions are not credited as completed transfers. There is no blind rollback
that reactivates a source while the destination might already hold addresses.
Retries re-check actual state and can complete partially applied operations.

Responses with missing/duplicate/foreign addresses, unsupported verification
versions, `Success: false`, or transport errors cannot complete the operation.
A timeout is an ambiguous outcome, not proof that no effects occurred. Local
handlers retain observed partial effects even when the caller's deadline expired.

Group deletion/removal uses the same verified release contract. If inventory
cannot be read, the configured address remains accounted for and the operation
reports an unconfirmed release. Promotion now honours refused MakePassive
responses, including self-promotion; an incomplete acquisition stops the final
completed-transfer broadcast. Promotion RPC acceptance remains asynchronous.

## Rollout and boundaries

Upgrade every member before relying on verified transfers. Old replies omit the
verification fields and are rejected. An old handler may already have applied
its request before returning an unverifiable reply, so a mixed-version move can
stop after releasing addresses. Retry after upgrading; do not read failure as
permission to restore the source blindly.

This is a verified cooperative transfer, not an ownership lease or fencing
protocol. The existing majority/operator policy for takeover without a reachable
incumbent is unchanged. An empty source means that policy has already been
chosen by the caller, not that release was verified. Concurrent reconciliation,
configuration changes and stale asynchronous workers can still change addresses
after the inventory snapshot (END-2698). Fencing remains END-2631. This change does
not make the whole promotion worker a durable transaction or provide a completion
RPC to replace its existing asynchronous acceptance response.

## Validation

- Full internal, package, command and unit suites with the race detector.
- Vet and Linux daemon/CLI compilation.
- Real loopback gRPC tests for refused/unavailable/legacy sources, partial
  release/acquisition, malformed receipts, invalid plans and idempotent retry.
- Tests that failed inventory reads cannot delete configured addresses or produce
  a partial snapshot, and that self-promotion stops on demotion refusal.
- Linux kernel test using disposable veth interfaces (explicitly enabled in CI): real RPC release,
  acquisition, wrong-interface refusal, repeated acquisition and final release.
  It uses a shared isolated network namespace and disables background monitor
  reconciliation; it is not the multi-node partition/fencing acceptance test.

Run the kernel test only in an isolated container with NET_ADMIN, NET_RAW and
arping installed. Cross-compile the internal/server test binary for that
container's architecture, then run it with `PULSEHA_TRANSFER_KERNEL_TEST=1` and
`-test.run '^TestIPTransferKernel$'`. The test creates and removes its own links.

The ordinary active-active integration fixture checks configuration and healthy
rebalance eligibility. It runs without NET_ADMIN, and its GetActiveIPs helper can
fall back to configuration, so it is not used as proof of acquired addresses.
The dedicated container test in CI supplies that kernel-level evidence instead.
