# Status and run-list latency

`GET /api/status` and MCP `system_status` read the most recent background
observations of upstream SSH authentication, Kubernetes discovery and OpenBao
login/seal state. Requests never initiate these probes, join an in-flight probe,
or refresh an expired observation. Before the first sample, diagnostics report
`unavailable`. `upstream_info[].checked_at`, `cluster_checked_at` and
`secret_store.checked_at` are UTC completion timestamps; a retained result keeps
its original time, including during an outage or a blocked refresh.

Upstream, Kubernetes and OpenBao diagnostics start immediately and wait sixty
seconds between sampling cycles by default. Configure `statusProbes` in Helm:
`upstreamIntervalSeconds`, `clusterIntervalSeconds` and
`secretStoreIntervalSeconds` (10–3600 seconds), and `upstreamFailureThreshold`
(1–10, default 3). The corresponding serve flags are
`--status-upstream-probe-interval`, `--status-cluster-probe-interval`,
`--status-secretstore-probe-interval` (Go durations, 10s–1h), and
`--status-upstream-failure-threshold`.

SSH authentication is shared across upstream organizations with the same host,
port, SSH username, resolved private-key contents and host pins. Local paths,
different identities and case-sensitive IPv6 interface scopes remain distinct.
Key projection, shared-key fallback and pin rotation are bound to the exact
credentials sampled; changed bindings start a fresh observation. Removing or
archiving an upstream removes its retry history.

Each endpoint independently backs off after failures: twice the interval, then
four times, capped at five minutes (rounded down to whole cycles), or the base
interval when longer. Retry due times are measured from sample completion and
checked on background cycles, so slow samples can defer a due attempt; there
are no catch-up connection bursts. A single failure never changes a previously
healthy endpoint to `unavailable`; three consecutive failures do by default.
Success resets the streak and interval. Before any successful observation, an
endpoint with fewer than the threshold failures reports `checking`; an
unsampled endpoint remains `unavailable` with no timestamp. Per-upstream
`last_error`, `consecutive_failures` and `checked_at` expose failures even while
the retained state is `ready`, and timestamps do not advance during backoff.
The dashboard shows the last error and check time beside the diagnostic state.

SSH concurrency remains four, with a five-second per-endpoint and fifteen-second
aggregate deadline. Cluster discovery has a five-second deadline; OpenBao login
and seal classification share ten seconds. All samplers are canceled and joined
before the database closes. These diagnostic states do not authorize mutations:
readiness requires process recovery, storage and durable SSH configuration;
push acceptance, mirroring and promotion use their actual transport and policy
checks independently. An unavailable diagnostic never refuses those operations.
Actual transport, credentials, audit or policy failures still can. Scope
filtering withholds other upstreams, global admission budgets, audit details and
secret-store configuration. Archived-only upstreams are not probed.

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
