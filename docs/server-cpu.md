# Server CPU under queue and event load

The server requests **500m CPU** and has **no CPU limit** by default. This
reserves scheduling capacity while allowing short bursts during MCP event
fan-out, queue changes and TLS handshakes. Memory and ephemeral-storage limits
remain 1Gi and 2Gi. The mandatory control-plane PriorityClass is unchanged;
priority does not remove a container's CPU quota.

The request is a starting point for a busy single-node install, not a capacity
guarantee. The load measurement below used about 41% of that request after the
fix. Monitor CPU usage and MCP latency on the installation, particularly during
checkouts, publication and bursts of new connections. Raise the request if the
server's sustained demand exceeds it. Node contention and external services can
still delay requests even without a container CPU limit.

## Overrides and upgrades

Normal Helm resource values remain configurable. For example:

```yaml
resources:
  requests:
    cpu: 750m
  limits:
    cpu: 2000m
```

A CPU limit is optional. A previously saved explicit limit remains an override;
clear it with `oberth upgrade --set resources.limits.cpu=null`. Likewise, clear
an old request override or explicitly set `resources.requests.cpu=500m` to use
the new reservation. An omitted or null CPU limit renders no CPU limit field.
A namespace LimitRange can inject a default limit; inspect the admitted Pod,
not just the Helm values. Retain required memory and ephemeral-storage bounds.

## Measurement and scope

`TestMCPUnderQueueLoad` uses the real scheduler, file-backed SQLite store, agent
event recorder and HTTPS MCP handler. It holds 10 running Workflows, queues 64
additional branches with an administrator hold, seeds the default 10,000-event
retention, and runs eight `events_wait` subscribers. Synthetic events arrive at
10/s and `run_list` requests at 20/s; every tenth list request closes its TLS
connection. The client pool can retain all subscriber connections. Each list
call and nonempty event delivery must complete within one second (scaled by
the repository's test timeout helper under instrumentation). Every subscriber
must receive events, and the exact run counts are checked before and after.

Git and the execution backend are fakes. The hold keeps the queue stationary;
there is no checkout/publication churn during the measured phase. Authentication
uses the test actor over real TLS. These results isolate server work, including
client CPU in the same test process; they do not reproduce a production forge,
Kubernetes API, mutual-TLS credential lookup, or the reported connection resets.
They are not an upper bound for those workloads.

The retained log was being scanned by `SELECT MIN(id), MAX(id)` on every event
page read, including every subscriber wake and periodic fallback poll. Two
scalar aggregates in one statement now seek the rowid endpoints and share one
read snapshot. No event state is cached, and cursor, retention-gap, scope and
fallback-poll behavior is preserved.

A 30-second Linux/arm64 run on an Apple M5 Pro Docker host, Go 1.26.10,
`GOMAXPROCS=2`, and a one-CPU container quota measured:

| Metric | Before | After |
| --- | ---: | ---: |
| Sampled CPU / elapsed time | 406m | 204m |
| CPU attributed to event bounds | 6.11 s | 0.14 s |
| `run_list` samples | 600 | 600 |
| MCP p50 | 11.3 ms | 8.76 ms |
| MCP p95 | 20.0 ms | 10.6 ms |
| MCP maximum | 38.3 ms | 38.8 ms |

The same final workload without a CPU quota also passed all 600 requests:
204m sampled CPU, 8.84 ms p50, 10.2 ms p95 and 27.5 ms maximum. This steady
workload fits below one CPU after the fix; it does not demonstrate the latency
of a production burst that exhausts a one-CPU quota. The uncapped default
provides room for those bursts.

CPU is the sum of pprof samples divided by the measured phase duration, not a
container CPU accounting counter. The scheduler's periodic reconciliation and
event sweeps were small in this stationary workload (at most 0.04 s sampled
CPU each after the fix); the 15-second dispatch reconciliation did not register
samples. TLS handshakes accounted for 0.08 s in each run. Git cost is excluded
by the fixture. These low-frequency paths need separate profiling during churn.

The independent Darwin/arm64 lookup benchmark demonstrates retention scaling:

| Retained events | Before | After |
| --- | ---: | ---: |
| 10,000 | 773 µs/op | 7.44 µs/op |
| 1,000,000 | 79.6 ms/op | 7.47 µs/op |

The query-plan regression requires two indexed searches and refuses a full
agent-event table scan. Semantic tests cover empty, singleton, sparse, appended
and deleted bounds, alongside existing retention and cursor tests.

## Reproducing the measurement

Run the ordinary regression with:

```sh
go test ./internal/service -run '^TestMCPUnderQueueLoad$' -count=1 -v
```

For a profile excluding fixture construction, use the service test binary's
registered test-only flags (these are not server or CLI flags):

```sh
go test ./internal/service -run '^TestMCPUnderQueueLoad$' -count=1 -v \
  -server-load-duration=30s -server-load-profile=/tmp/server-cpu.pprof
go tool pprof -top -cum /tmp/server-cpu.pprof
go test ./internal/store -run '^$' -bench '^BenchmarkAgentEventBounds$' -benchmem
```

For a quota comparison, build the test binary first and run it in a Linux
container with `--cpus=1`, then without that option, keeping `GOMAXPROCS=2` and
the same workload. For an old-code comparison, copy the load test into a
checkout of `132e395f` and run the same command. Report setup, elapsed phase,
request count, percentiles and errors, not only average CPU.
