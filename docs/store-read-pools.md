# Store reader isolation

Live file-backed stores use WAL, one primary writer (`_txlock=immediate`),
and an eight-connection read-only ordinary reader pool. Repository, run-list,
run-detail, step-result, queue-pattern and next-queued-run reads use that pool.
`NextQueuedRun` is a peek; claiming work still selects and updates atomically
on the writer. Reads inside mutation transactions stay in those transactions.

Authentication, audit scans, idle sampling and readiness use separate reserved
readers. In particular, readiness has a single read-only connection and does
not wait for the ordinary pool or primary writer to become idle. Reader
connections use `mode=ro` and `query_only=1`, without immediate transactions.
Opening readers uses the filename resolved at startup, without querying the
writer again. Private immutable inspection handles retain their existing
connection policy.

Bearer authentication reads current identity, scope and revocation before
enqueueing any due token-use timestamp update. One Store-owned background worker
performs UPDATE-only writes on the serialized writer, with the existing active
and unrevoked predicates. It never acquires authentication admission or readers,
including after a no-op. The queue holds at most 64 distinct tokens plus one in
flight; pending IDs are deduplicated. It records accepted use time rather than
worker completion time. Failed writes start a global one-minute cooldown;
arrivals and queued uses are discarded during that cooldown. Saturation and
shutdown can also drop telemetry. Fixed counters for attempts, failures, queue
drops and cooldown drops are published through the administrator metrics endpoint.
The timestamp is best-effort; authentication never waits for its writer slot or
commit, including first use and the one-minute refresh boundary. Store close
cancels and joins the worker before closing SQLite. Cancellation interrupts a
pool-slot wait; an external SQLite busy wait may still last to its busy timeout.

The primary still performs startup PRAGMAs, migrations and schema/protocol
validation. `UPDATE`/`INSERT ... RETURNING` statements are writes, even though
Go executes them with `QueryRowContext`. Audit mutation fences deliberately
sample `data_version` on the same physical primary connection; moving those
samples to another connection would break the fence's cache invariant.

`TestAPIReadsWhileWriterTransactionHeld` checks public reads while a real WAL
write remains uncommitted, including committed-state visibility.
`TestReadinessReaderSurvivesWriterAndMappingPoolSaturation` also exhausts all
ordinary reader connections while holding the writer.

For local writer-hold measurements, run:

```bash
go test ./internal/store -run '^$' \
  -bench '^BenchmarkWriterHoldUnderAPIReads$' -benchtime=16x
```

The test-only driver records successful `BeginTx` through commit/rollback;
connection acquisition and BEGIN wait are excluded. Four ordinary-reader
workers exercise repositories, recent runs, priority patterns and queue peeks.
`max-held-ns` covers every measured transaction; `p95-held-ns` uses at most the
last 1,024 samples. `reader-calls` includes warm-up; `writer-wait-ns` is the
writer pool's cumulative connection wait, not transaction hold time. Results
reflect the local fixture, hardware and contention, not an installation-wide
latency bound or a census of every write operation. Busy deadlines and external
witness/admission gates remain independent of this routing change.

## Disposable store test fixtures

The store test helper sets SQLite `synchronous=OFF` on its private writer
connection after opening it. Fixtures keep file-backed WAL, foreign keys and
the production connection limits. Production initialization, migration,
inspection and recovery tests that open stores directly retain `synchronous=FULL`.
A regression test opens the same fixture file through the production entry
point and checks that FULL is still selected there. This fixture optimization
does not test power-loss durability.

SQLite has no PostgreSQL `fsync`, `synchronous_commit` or `full_page_writes`
settings. There is no new production setting or environment override.

Branch `test-store` and release `release-test` mount a private 768Mi memory
`emptyDir` and pass `OBERTH_TEST_STORE_TMPDIR` to the store test binary. Its
test entry point sets both `TMPDIR` (for `os.MkdirTemp`) and `GOTMPDIR` (for
Go 1.26 `testing.TempDir`). Other test binaries and the Go build cache keep
their original directories. Without that variable, local tests keep their
normal temporary directory; a caller with sufficient memory can select a
private tmpfs directory explicitly.

The tmpfs is included in the existing container memory envelope: 4Gi for
`test-store`, with a 2GiB Go heap target, and 8Gi for `release-test`, with its
4GiB target. CPU requests are 971m and 1000m respectively, with limits of
2 and 4 CPU. The store lane and full release race suite use a 55-minute
per-package deadlock bound; the service lane uses 45 minutes. The combined
v0.17.84 candidate's whole-package race measurements were 2,084.275 seconds
for store and 1,765.448 seconds for service, with two package workers,
GOMAXPROCS=2 and no scheduler pauses. The bounds apply the same 1.5× measured
wall-time rule as the earlier store sizing, rounded up to five-minute
increments. The earlier 35-minute store bound left less than 16 seconds of
margin on this combined source, and service exceeded its earlier 20-minute
branch bound.

Other branch race lanes retain their 20-minute bounds. These package
watchdogs allow slower deadlock detection; individual test contexts and
assertions retain their existing bounds. Go worker counts remain bounded.
Native wall times do not establish CI resource headroom: qualify the combined
source under the declared container CPU, heap and tmpfs envelopes.

The contention test first checks committed reads
while holding an uncommitted writer transaction, with a generous scaled
one-minute deadlock bound. After releasing that transaction, it keeps the three
receive/status lookups running until all writers finish. A successful initial
lookup releases the writers, and every round requires a successful lookup after
that release. It still performs all 16 × 32 status/step write pairs and checks
every result, without a throughput deadline or lookup latency assertion.

## Telemetry-retention writer exception

Resource-usage retention is the deliberate exception to the single primary
writer pool. It selects candidates through an ordinary WAL reader, then opens
its own one-connection writable pool for at most 100 rows per DELETE and 1,000
rows per sweep. It does not acquire a primary pool slot. SQLite still serializes
file-wide writes: while a retention batch commits, a primary writer can wait
inside its busy handler while holding its own pool slot. The sweeper's 25ms busy
wait and one-second context favor yielding; a stalled filesystem commit cannot
necessarily be interrupted at the context deadline.

A deleting retention commit counts as an outside commit to the primary's
`PRAGMA data_version`, even though the Store initiated it and it touches no
audit rows. The next local audit mutation gate invalidates prefix-cache reuse
and scans the chain again. A sweep deleting nothing does not reset that cache.
Gates interleaved with multiple committed batches can require repeated local
verification; retaining a separate writer and avoiding these invalidations
cannot both be assumed. No exception bypasses the audit fence or classifies
another connection's table-specific writes as trusted. See
[audit-cache invalidation](audit-verification-cache.md#telemetry-retention-commits).

The ordinary sweep interval is 10 minutes by default. SQLite BUSY/LOCKED is a
DEBUG-level skip rather than `retention.LastError`; resource telemetry alone
retries after one minute (or the configured sweep interval if shorter), without
rescanning log/artifact evidence. Other failures remain errors. Under continuous
writes, every retry can yield with no deletion; age/count targets may remain
exceeded indefinitely until a writer gap is available. Partial committed batches
remain deleted, and later sweeps select the oldest remaining rows. No retry
extends the 25ms busy wait, one-second pass or batch bounds.
