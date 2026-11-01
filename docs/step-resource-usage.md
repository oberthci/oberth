# Step resource measurements

Counters are nullable, self-reported advisory telemetry. Build code can forge
its own termination message; never use these counters for admission or size
grants. Future aggregations must saturate on overflow. No aggregation is
currently performed. CPU durations are nanoseconds (divide by 1e9 for seconds).

The server's single-architecture executable is streamed and SHA-256 verified
once into each ordinary source PVC. Wrapped container/script Pod templates
add `kubernetes.io/arch` matching the server's `runtime.GOARCH` through
`podSpecPatch` strategic merge, preserving administrator workflow/parent selectors. Protected
KVM workflows and unsupported leaves remain unwrapped. This adds no hostPath,
capabilities, ServiceAccount token or RBAC.

Instrumentation runs in the main container's cgroup and consumes its budget.
The current wrapper is the full server executable: all linked Go package
initializers run before `internal-step-exec` dispatch. It is not a small shell
wrapper. New container/script leaves are therefore measured only with an
**explicit container memory limit of at least 256Mi**. Smaller or unspecified
limits keep their original command, resources and scheduling constraints;
usage remains null. A request is not a limit, and inherited/defaulted limits
are not sufficient evidence of headroom. Oberth neither raises a limit nor
adds a memory request. This cutoff is an eligibility policy, not a guarantee
that the remaining budget is sufficient for arbitrary user code.

At admission the server logs a warning naming the Workflow, template, limit
and instrumentation floor. The warning is also retained on the template as
`oberth.ci/step-usage-memory-warning` (visible on the Workflow and its Pod).
The server stamps `oberth.ci/step-usage-memory-policy` on new Workflows so
restart/adoption preserves both the new policy and already-running older
wrapped specs. Existing Pods are not rewritten: resubmit a failed tiny step.
Skipping usage instrumentation does not remove a repository's explicit
`oberth secretstore exec` or Argo's emissary; those still consume memory.

Run the admitted tiny-step command through the real Argo v4.0.8 emissary
under a 32Mi cgroup with swap disabled:

```sh
hack/test-step-usage-memory.py --argoexec /path/to/linux/argoexec \
  --receipt /path/to/receipt.json
```

Docker runs locally; the cluster gains no permissions. The receipt includes cgroup
peak/events, process RSS and container exit/OOM state. The short shell script
holds a 4MiB variable so the test exposes overhead that an empty `true` can
hide under reclaim. Supply `--baseline-binary` to compare the same command
forced through the previous full-server wrapper. The binary, emissary and
BusyBox image must match the selected Linux architecture; the default fixture
image is arm64. `--image`/`--platform` support other local targets. Missing
tools or a failed container are errors, not skips or successful evidence.

On 2026-10-28 a Linux/arm64 Go 1.26.10 release-flag build on v0.17.61
measured roughly 45–48MiB wrapper RSS without memory pressure, including
reclaimable executable pages. Argo plus the wrapper peaked at 139–141MiB in
a 256Mi cgroup; this motivated the 256Mi eligibility floor. The 32Mi short
script passed 10/10 admitted runs; the identical forced-wrapper runs were
OOMKilled/137 10/10. These are synthetic local measurements, not a capture
of a production Pod. Even an empty command can hide the regression because
Linux reclaims executable pages aggressively under pressure.

The wrapper forwards catchable signals to the child process group, except
SIGCHLD and Go runtime SIGURG, like emissary. SIGKILL and SIGSTOP cannot be
caught. Child signal death returns 128+signal (SIGSEGV is 139). A command that
cannot start returns shell-style 127, a failed step rather than emissary's
executor Error. An existing nonempty termination message written by the step
is preserved; usage is then unknown. A wrapper killed before it reports also
leaves usage null. See AGENT-CONTRACT.md for measurement scope and other gaps.

## Seed cost measurement

`hack/benchmark-step-seed.py BINARY NAMESPACE POD --samples 10` measures only
the binary delivery addition, with a disposable seed Pod mounting its writable
PVC at `/seed`. The container must be named `seed` (or pass `--container`) and
provide sh/cat/chmod/sha256sum. The script overwrites `/seed/oberth-bin/oberth`,
streams with SHA-256 calculation, then verifies through a second Pod exec,
matching SourceSeeder's commands. It also times two empty execs as a control.
It creates no cluster resources and changes no RBAC. Use an isolated Pod/PVC;
never point it at a running server or real source checkout.

On 2026-10-27, ten local kind v1.37.0 samples with the chart-pinned BusyBox 1.37,
a fresh local-path PVC, UID/GID 65534, no capabilities, a read-only root and a
1 CPU/128 MiB limit measured the 51,576,994-byte Linux/amd64 release-flag build:

| Operation | Median | Range |
| --- | --- | --- |
| Stream and hash | 0.315 s | 0.301–1.604 s |
| SHA-256 readback | 0.230 s | 0.219–0.241 s |
| Total binary seed addition | 0.552 s | 0.529–1.831 s |
| Two empty execs | 0.139 s | 0.127–0.150 s |
| Addition minus empty-exec control | 0.415 s | 0.380–1.694 s |

Every readback matched. Each sample removed the prior file before timing,
as production creates a new per-run file. OS/storage caches were warm after
the first sample. The amd64 binary was copied, not executed on the arm64 node.
These measurements include kubectl startup and the local API exec transport;
they exclude Pod scheduling, PVC provisioning and source archive delivery.
Production uses the existing Go exec client rather than kubectl processes;
network latency, remote PVC storage and CPU contention can change the cost.
Run the shipped harness from tuxbox's server network to measure that deployment.

## External sampler coverage (#1216)

The opt-in kubelet/cAdvisor sampler records each current container incarnation
in verified Workflow step Pods: ordinary app/sidecar containers (including Argo
`wait` and mainless ContainerSets), ordinary/restartable init helpers, and
ephemeral containers. CPU cumulative time/interval peak, memory current/high-water
peak and filesystem IO counters retain full Pod UID/runtime container ID/cgroup
binding and timestamp freshness checks. Termination messages and progress
telemetry never supply these observations or admission evidence.

Kubernetes creation, kubelet pod start, container start/finish and fully known
terminal pod finish times accompany the observations. Timing-only running rows
can survive a failed scrape with zero metric samples. A final killed observation
merges without erasing earlier measurements. Missing start/finish times and
unobserved/garbage-collected incarnations remain unknown.

For workflows declaring VM resources, two additional bounded inventories can
bind a launcher through Workflow controller UID -> VMI UID -> launcher controller
UID. The VMI operation annotation must bind its original resource step Pod UID.
All launcher containers use the same cgroup checks. These are launcher totals
(including QEMU/guest consumption), not a measurement of guest versus hypervisor
cost. Missing original resource Pods/VMIs leave that binding unknown.

The existing `/stats/summary` response adds these optional storage/pod scopes:

- Per-container writable-rootfs and log used-byte peaks, accepted only when the
  summary container creation time is no later than the Kubernetes running start.
  After a restart it must also be no earlier than the known previous finish;
  an unknown previous finish leaves container storage unknown.
- Pod cgroup CPU interval-rate and memory-usage peaks, including pod overhead;
  they are measured directly, never a sum of independent container peaks.
- Existing pod ephemeral-storage and IO PSI counters.
- Pod PVC used-byte peaks: a sampled sum over distinct referenced PVCs only when
  every claim has fresh matching summary volume stats with equal source times. Incomplete/unsupported
  stats stay null; generic ephemeral volumes also leave the aggregate unknown.
  Shared claims must not be added across Pods. Differently timed claim stats remain unknown rather than a synthetic simultaneous total. This is mounted
  filesystem usage, not per-run attribution on a shared filesystem.

The read-only, repository-authorized MCP call remains:

```json
{"name":"repo_usage","arguments":{"repo":"oberth","burn":"checks","step":"test","limit":1000}}
```

It returns the existing main-only `steps` and separate `containers`, deduplicated
`pods`, and `overlap` summaries. Container and pod metrics report known-value
count/p50/p95/max; p95 is across incarnation/pod peaks, not time samples.
`overlap.per_run_peak_pods` reports quantiles of each run's maximum overlapping
kubelet pod lifetimes in the selected rows. Intervals are half-open; unknown
finishes end at the last observed running container or a known container finish,
not at the report's current time. Missing timing is excluded and counted through
`pods`/`timed_pods`; `open_pods` lacks a fully known pod finish. Raw timing fields
also appear in the existing `run_get` measured observations. Resource peaks are
not summed to invent simultaneous whole-run demand. Filters/truncation can make
both the overlap and usage population incomplete; coverage is never claimed.

The source cannot separate guest/hypervisor memory or CPU, inspect guest files,
measure raw block-volume used space, attribute shared PVC use to a run, or recover
missed peaks/GC'd containers. Platform Pods, independent source-seed/artifact
helpers outside Workflow supervision and CDI importer Pods without the verified
step/VMI chain are outside this collector. They still need separate observations;
stage 2 remains blocked on representative, complete coverage.

Collection overhead is bounded: default 15s (existing configurable 1–30s), two
shared node HTTP GETs per interval, each at most 16 MiB; at most 32 configured
nodes, two concurrent fills, four seconds per fill and five seconds per run
sampling pass. New scopes add no kubelet requests. A shared immutable index
covers at most 65,536 retained metric lines and 4,096 summary Pods, avoiding a
whole-node metric scan per container. Each pass reads at most 2,048 Workflow
Pods; VM workflows additionally read at most 2,048 VMIs filtered by Workflow label and 2,048 launcher Pods.
The launcher inventory still spans the namespace; owner UIDs remain authoritative.
Pagination is deliberately not followed; coverage can be incomplete. Pods with
more than 128 declared containers or container statuses are excluded;
container/volume storage lookups also cap each relevant list at 128; a pass
visits at most 4,096
uncached container candidates and writes batches of at most 256. Index/observation cost
and extra VM API reads are the added overhead; production CPU/RSS/IO cost has
not been qualified on tuxbox.

Storage remains one upserted JSON row per run/node/Pod UID/runtime container ID,
not one row per poll. Existing incarnations remain updatable at the 4,096-row
per-run cap; additional incarnations are omitted, so population coverage remains
unknown. JSON is capped at 8 KiB: at most 32 MiB observation payload per retained
run, excluding keys/indexes/SQLite WAL. Running timing updates may write every pass even with cached/unavailable
metrics. Unchanged terminal observations execute no UPDATE, and each sampling
session remembers at most 4,096 successfully recorded finished container IDs to
skip their subsequent scrapes/SELECTs/merges. A known terminal Pod finish that
arrives after its containers terminate is recorded once, then suppressed again;
this transition also works when the cache is full. A failed recording is not cached.
A server restart rebuilds this suppression set on the first pass.

Telemetry retention runs through the existing evidence sweep timer (default
10 minutes), including with log/artifact age/budget policies disabled. It targets
90 days since last observation and the newest 100,000 rows globally, deleting
oldest first. Each pass selects at most 1,000 rows and issues autocommit DELETEs
of at most 100 rows on a separate one-connection pool; it never borrows the
primary connection. The pass has a one-second context and a 25ms SQLite busy
timeout. File-wide writer serialization and filesystem commit time still apply.
Rows renewed after selection are preserved. Backlog, failures and arrivals
between sweeps can temporarily exceed the targets; these are retention targets,
not a hard database-size ceiling. At target occupancy the JSON payload ceiling
is 100,000 x 8 KiB (about 781 MiB), plus keys/indexes/SQLite/WAL. Backlogs drain
at most 1,000 rows per sweep. SQLite BUSY/LOCKED is a DEBUG skip, with a telemetry-only retry after one
minute (or a shorter configured sweep interval). Other errors appear through
retention status/logs. Continuous writes may prevent all retries from pruning
until a gap appears, so targets can remain exceeded indefinitely. Retention
commits also reset the primary audit cache and force fresh verification; see
[reader/writer trade-offs](store-read-pools.md#telemetry-retention-writer-exception).
Run history is not routinely deleted, so an FK cascade is not a retention policy.
Reports read 1,000/default–5,000/max rows, return at most 128 groups in
each of `steps`, `containers` and `pods` with separate truncation flags; run views
remain capped at 256 observations, prioritizing workflow/main rows before helpers.
More containers consume those existing caps. Limits and `observations` count
container rows, not Pods; all report populations remain explicitly incomplete.

Schema 37 guards the format and adds the observation-age retention index;
existing table/index definitions are unchanged. New JSON observations use version 2; version-1 main rows remain
readable and upgrade on observation without duplication. Older binaries reject
version-2/non-main rows, so schema 37 prevents opening them. The migration applies
even with sampling disabled and follows the existing forward-only backup/restore
contract. Helm rollback alone stops the old server with schema-too-new; rollback
requires restoring the pre-upgrade database backup. See the [schema-37
upgrade/rollback note](schema37-upgrade-note.md). No admission, authored requests, limits, weights or workload specs are
changed. Low/unknown/OOM-censored observations cannot authorize smaller resources.

The optional storage field shapes and pod-cgroup scope follow the pinned
[Kubernetes v1.36.5 summary API](https://github.com/kubernetes/kubelet/blob/v0.36.5/pkg/apis/stats/v1alpha1/types.go).
Availability depends on the runtime/volume driver; the
[CRI stats provider](https://github.com/kubernetes/kubernetes/blob/v1.36.5/pkg/kubelet/stats/cri_stats_provider.go)
can omit statistics it cannot obtain. No new dependency on these packages is added.

R2 isolation: the Workflow sampling goroutine and shared node-fill goroutine
recover scraping/recording panics so optional telemetry cannot crash the server.
The session stops on panic; shared fills publish unavailable snapshots and
release waiters/gate slots. Panic payloads are not logged. Node-clock timing
fields more than two seconds ahead become unknown without dropping independent
valid usage. A partial batch at a canceled pass deadline is still not retried
under a fresh context; batch-error isolation and previous-incarnation
LastTerminationState recording also remain follow-up work.
