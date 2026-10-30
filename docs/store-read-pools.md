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
