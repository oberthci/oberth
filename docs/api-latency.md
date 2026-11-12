# Status and run-list latency

`GET /api/status` and MCP `system_status` read the most recent background
observations of upstream SSH authentication, Kubernetes discovery and OpenBao
login/seal state. Requests never initiate these probes, join an in-flight probe,
or refresh an expired observation. Before the first sample, diagnostics report
`unavailable`. `upstream_info[].checked_at`, `cluster_checked_at` and
`secret_store.checked_at` are UTC completion timestamps; a retained result keeps
its original time, including during an outage or a blocked refresh.

Upstream and cluster sampling starts immediately and ticks every ten seconds;
OpenBao ticks every thirty seconds. Upstream SSH concurrency is four, with a
five-second per-upstream and fifteen-second aggregate deadline. Cluster discovery
has a five-second deadline; OpenBao login and seal classification share ten
seconds. Samplers are canceled and joined before the database closes. Archived
repositories retain their archive behavior, and an upstream configuration/key
change invalidates its cached observation. Scope filtering still withholds other
upstreams, global admission budgets, audit details and secret-store configuration.
These diagnostics do not authorize mutations or change admission/readiness gates.

Run-list startup observations and immutable queue-demand reads refresh in the
background on demand. At most four refreshes run at once, each with a two-second
budget, with 512 retained entries, a bounded FIFO of at most 512 pending keys
and a ten-second refresh interval. Concurrent misses coalesce; queued refreshes
continue across the page without another poll, so later rows cannot starve. Cached startup results retain `scheduling.observed_at` and update
`age_seconds`. A cold observation is omitted; admission reasons and queue order
still come from current process state and the database snapshot. Successful demand
results retain the existing immutable run/SHA cache; missing demand stays unknown.
Cache keys bind the attempt, workflow, update time and authorized repository view.
API shutdown cancels and joins refresh work. Run detail reads keep their direct
bounded observations.

## Query plans

The grouped run query materializes narrow heads once from
`runs_latest_ref_idx`, ranks those heads and only fetches wide rows for the page.
It scans no historical step results and does not perform application-level
per-history-run joins. Exact repository/ref predicates are emitted only when
supplied, allowing an indexed repository search instead of an optional-OR scan.
Earlier-attempt counts, terminal cursors, scope restrictions and scheduler queue
order keep their existing semantics.

The API-read migration adds covering `runs_counts_idx`, partial
`runs_active_ref_idx` for correlated active-successor checks, partial
`runs_terminal_end_idx` for terminal pages, and `audit_actions_type_idx` for the
latest backup/typed audit record. Historical priority and repository projections
still use indexed scalar lookups, bounded by returned rows.

## Reproducing the measurement

`TestAPIHandlersLargeHistoryLatency` builds 50,000 historical runs with 2 KiB
error text, 100,000 historical steps, 2,000 refs, twenty repositories and four
upstreams, then adds fifty queued and ten running runs. It exercises real bearer
authentication, service projection, SQLite and HTTP serialization. Forge,
Kubernetes, OpenBao and startup/demand observers deliberately block until their
contexts end. Cold, in-flight and stale snapshots must all serve each HTTP
handler and MCP status in less than 500 ms; both flat and grouped pages must keep
all sixty active runs alongside the hundred terminal rows.

```sh
go test ./internal/app -run TestAPIHandlersLargeHistoryLatency -count=1 -v
go test ./internal/store -run 'TestAPILatencyQueries|TestLatestRefLarge' -count=1 -v
go test ./internal/app -run '^$' -bench BenchmarkAPIHandlersLargeHistory -benchtime=2s -cpuprofile=/tmp/api-latency.cpu -o /tmp/api-latency.test
go tool pprof -top -tagfocus='handler=/api/status' /tmp/api-latency.cpu
go tool pprof -top -tagfocus='handler=/api/runs' /tmp/api-latency.cpu
```

The CPU profile labels isolate handler work from fixture construction.
`TestAPILatencyQueries` logs `EXPLAIN QUERY PLAN` and checks the relevant indexes;
`TestAPIReadIndexesUpgradePreservesHistory` checks migration replay, retained
runs/steps, audit continuity and receipts. Probe and list-cache tests cover cold
reads, refresh coalescing, identity changes, age and cancellation. Local fixture
measurements characterize the tested machine; they do not claim live deployment
latency or CDN behavior.
