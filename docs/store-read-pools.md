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
