# Local audit mutation verification

Local-only and timestamp-only mutation gates cache a verified audit prefix in
the live `Store`. Witnessed gates continue to verify the complete protected
state against recovered external history. This cache is an optimization of
chain verification, not an authorization result: pending publications, the
latest checkpoint and its chain reference, the manager's prefix pin, external
receipt verification when configured, and the final writer fence still run
on every gate.

## Trust boundary

The cache contains the last successfully fenced head ID and SHA-256, the
complete latest anchor (including receipt), a schema cookie, the time of the
last full gate scan, and a key identifying the primary physical SQLite
connection, its `data_version`, recursive-trigger setting, and audit hash
domain. The cache and prefix pins exist only in process memory. Nothing is
persisted or recovered as a trusted cache after a restart.

SQLite's `data_version` changes for commits made by **other connections** and
does not change for the observing connection's own commits. The Store owns
one primary connection. Ordinary application writes on it append audit rows;
they cannot update or delete a verified prefix. Cache eligibility requires
the binary's canonical audit table and unconditional update/delete guards.
Writable connections enable `recursive_triggers`, making the delete guard
apply to `INSERT OR REPLACE` too, including replacements from other triggers.
Disabling that setting disables cache reuse. A modified startup guard never
becomes a cache trust root even if the current rows pass a full hash scan.

The startup schema fingerprint and guard eligibility are captured atomically.
A later schema fingerprint must match that baseline before a full scan can
seed the cache. Schema changes that differ from the baseline leave the Store
on full verification until the original schema is restored or the Store is
reopened. The fingerprint covers all schema objects, including triggers.

Every other database connection is untrusted for this optimization, including
another Oberth Store or an administrator using SQLite directly. Its commits
invalidate the cache even if they only append valid rows or touch unrelated
tables. Restoring trigger definitions and resetting `schema_version` does
not conceal such a commit: `data_version` belongs to the process-owned
connection, rather than to a mutable SQL table. No database-resident counter
or unsigned checkpoint is trusted to attest that the prefix is unchanged.

The assumptions remain the normal live SQLite/WAL and process-integrity
assumptions: the application owns its connection and SQL, and database edits
use SQLite's commit protocol. Arbitrary memory corruption or rewriting a
live SQLite file outside that protocol is not a supported restore mechanism.
Offline restores require reopening the database. Replacing the pathname's
inode while the Store remains open is detected and refuses gates until the
Store is reopened, avoiding decisions that mix old writer and new reader
files. A missing file or a metadata/SQLite error also refuses the gate.

## Verification and invalidation

The first local gate on every newly opened Store scans from genesis. Reuse
then checks the primary key before opening the read snapshot, pins that
snapshot by reading the schema cookie, and samples the primary key again.
Both samples must equal the cached key. The schema cookie, latest anchor,
and boundary digest must also match. This ordering prevents a commit around
snapshot creation from blessing an unseen prefix. A full scan records the
earlier key, so a commit racing that scan can at worst cause an additional
full scan on the next gate.

On the fast path the verifier reads only rows after the cached head and
recomputes their hashes, sequence numbers and predecessor links. It then
checks the previous in-process head and the caller's prefix pin. A valid
shortened or rebuilt chain cannot reset either pin. The cache advances only
after the external callback and final fence succeed. Errors invalidate reuse
while retaining the last successful head as a rollback pin. Gate callers are
serialized with a context-cancellable wait.

A full scan from genesis runs on:

- every process/Store restart and first local gate;
- any primary connection identity, external `data_version`, hash-domain or
  recursive-trigger setting mismatch;
- a schema-cookie or latest-checkpoint change;
- a missing/replaced cached boundary or head regression (followed by refusal
  if the previous verified prefix is absent or differs);
- a previous failed gate or other invalid cache;
- the first gate at least one minute after the last full gate scan, measured
  with a monotonic process clock.

The manager also retains its existing complete startup verification and full
scans on every periodic cycle. The one-minute gate expiry works independently
of that cycle's configured interval: it is checked at the next gate, not by
an additional background timer. Full gate scans are not skipped just because
a periodic cycle recently scanned the database.

A concurrent head advance makes the final fence refuse the current attempt
with `ErrAuditVerificationBusy` (also matching `ErrAuditStateChanged`). If only
the reader's version changed, up to two extra local snapshots can salvage the
same external proof; exhaustion also reports contention. The manager retries
these refusals automatically with backoff from 10 ms to 250 ms, under the gate's
overall 30-second default deadline, including its serialized queue wait. Each
fresh attempt repeats all local and configured external checks. External
verification runs once per attempt, and the head and complete checkpoint must
remain identical to its inputs. Own unrelated writes can reuse the prefix;
outside commits force the full scan.

Integrity failures, changed checkpoints, head regressions and file replacements
do not receive the contention classification and fail immediately. A retry
never grants permission without a new successful fence. A contention timeout
preserves both the transient classification and the context error; MCP mutation
requests report an unavailable service with a public reason to retry. As before,
the fence ends before the caller's application mutation begins; the gate does
not hold a writer across that mutation, backoff or network I/O.

## Detection guarantees and cost

An outside edit to an already verified row forces full hashing at the next
gate, including an edit made during its external callback. It is not deferred
until the periodic scan. A forged appended row is rejected by suffix hashing.
Thus caching does not delay prefix-tamper detection relative to the previous
full-scan local gate under SQLite's commit protocol. Signed checkpoints and
external continuity retain their existing checks. Local modes still cannot
prove the absence of a self-consistent whole-history rollback across process
restarts without rollback-external evidence.

Successful local gates advance the manager's prefix pin. Periodic local
cycles now check the previous pin before replacing it: a valid shorter chain
can no longer clear a rollback refusal at the next cycle (review F5).

With an unchanged eligible schema and no outside commits, gate chain work is
O(new rows), plus indexed boundary lookups and fixed snapshot/fence work. The
pending-publication checks retain their existing separate cost. Full scans
remain O(total rows), and outside writers intentionally trade performance for
fresh verification. The benchmark includes cached gates, an append plus gate,
and forced full gates at 1,000, 10,000 and 50,000 initial rows:

```sh
GOWORK=off go test ./internal/store -run '^$' \
  -bench '^BenchmarkLocalAuditGate$' -benchtime=20x -count=3
```

The benchmark asserts that cached and append cases perform only their warmup
full scan. Allocation counts provide an additional check independent of host
load; compare the forced-full case's growth with the stable cached cases.
