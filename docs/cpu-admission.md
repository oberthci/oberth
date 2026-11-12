# CPU admission

Since v0.21.10 Oberth reserves effective concurrent CPU requests before
submitting a Workflow. CPU limits remain burst ceilings. Existing running
Workflows keep their submitted resources through observation and cleanup.

## Defaults

Oberth sums allocatable CPU across Ready, schedulable nodes without
NoSchedule/NoExecute taints, reserving 20% for platform and host work. A 12-CPU
node provides a 9600m pipeline pool. Step requests are capped at 25% of the
largest eligible node's allocatable CPU (3000m on 12 CPU), including scripts,
container sets, init containers and sidecars. Limits stay unchanged.

Each remote target uses its own cluster and pool. A target pinned to one node
reads and sizes only that node; its explicit dedicated-node toleration applies.
Local and remote observations refresh every minute. A cordoned/tainted pool,
missing eligible node or denied node read holds admission for that pool with a
reason line while the server keeps serving. Recovery or capacity changes take
effect without restarting Oberth. Local sizing needs `get,list` on nodes;
pinned targets need `get` on their selected node. No watch, proxy or node write
permission is needed. Affinity and unrelated workloads still affect placement.

Missing container requests receive the usual defaults. Admission includes the
executor's 50m CPU and 64Mi memory per concurrent Pod, including resource-only
workflows. With workflow parallelism, exit/hook graphs reserve at most that many
largest template requests, scanning handler templates too. Without it, exit
handlers run after the main graph (a maximum rather than a sum); hooks add
concurrent work for each hooked invocation. Truly unbounded graphs reserve the
whole pool. Oversized fan-out is clamped for admission. For static fan-out with
no authored workflow parallelism, Oberth freezes a parallelism cap based on how
many largest Pods the pool fits, so Argo queues the remainder.

After five distinct successful fully covered runs for a template, main-container
sizing uses P95 of CPU time divided by container lifetime, adds 25%, and rounds
up to 10m. A measured request cannot fall below 10% of its authored request or
50m, bounded by the authored request and operator node cap. One run gets one
vote (its largest invocation average); instantaneous peaks do not size requests.
For example, 10m observed usage against a 1050m declaration sizes to 105m before
the executor allowance. Burst limits remain unchanged.

History is isolated by repository and execution class and bounded to the latest
128 started runs and 4096 usage rows. Incomplete, reset, restarted, saturated and
known CPU-limit-censored samples are excluded. Stored usage does not contain
CPU-throttling counters or CPU-pressure observations; no new collection is
introduced. Missing usable history retains capped declarations. The request and
parallelism policy is frozen before admission and retained for retries.

`admission.cpuEnabled: false` / `--admission-cpu-enabled=false` disables CPU
admission, node reads/refresh, request caps, history resizing and CPU-capacity
expiry. Other admission gates still apply. `admission.cpuHeadroomPercent` and
`admission.stepCPURequestPercent` correspond to `--admission-cpu-headroom-percent`
and `--step-cpu-request-percent`.

Pressure retains a 25% CPU and 40% IO hard threshold, with a 300-second maximum
deferral. A hard-pressure timeout admits at most one otherwise-fitting run per
300-second window; claims and releases do not renew that allowance. The CPU
budget, disk floor and administrator hold still apply. Unavailable PSI does not
disable CPU packing. Chart values are `admission.pressure.hardCpu`, `hardIo` and
`deferSeconds`, with the corresponding `--admission-pressure-*` flags.

## Ordering and bounded waits

Priority remains `low < standard < promotion < high`. Within one class,
repositories with fewer active runs go first, then least recent service and
FIFO. Ordinary preparation is bounded to eight prepared waiters and one
checkout preparing at a time. Higher-priority work can still be claimed after
that window fills. An unprepared claim holds only its own pool.

Each pool has one drain clock protecting its oldest capacity-blocked head.
Backfill is allowed for two minutes; afterwards, work behind that head cannot
refill its pool until the head starts or is withdrawn. Additional blocked
waiters do not reset the clock. Other pools keep advancing. This prevents
finite but overlapping small jobs from starving a whole-pool head indefinitely.

Argo receives priority -1 for low, 0 for standard, 1 for promotion/release, and
2 for high. Repository YAML cannot choose it. Argo's own parallelism and native
locks remain additional constraints.

An owned Pod waiting on `Insufficient cpu` is exempt from the stall timeout if
its effective CPU request can fit the largest eligible node. This includes
clamped fan-out and waits caused by outside workloads. Only a request exceeding
that node's allocatable CPU expires after `admission.stallTimeout` (10 minutes
by default), including later steps and observer restarts. Effective Pod requests
include init phases, restartable init sidecars, Pod-level resources and overhead.
When capacity is unknown, impossibility is not inferred. Identity-delivery and
other startup deadlines keep their separate bounds. Cleanup/API failures may
extend how long resources remain owned.

## Live credits and recovery

Unknown adopted demand is read from owned non-terminal Pods before dispatch.
A failed read reserves the whole pool and retries on the 15-second refresh;
credits never expire by age, including long-running workflows from an upgrade.
Pending and terminating Pods keep their charges; completed or foreign Pods do
not contribute. No resident Pods in an unfinished unknown Workflow is treated
as unknown, since the controller may not have created its work yet.

Held assistance uses the summed requests of all live Pods, including active
sibling branches, refreshed every 15 seconds. That live accounting continues
after an attempt resumes: resident Pods retain their charge throughout the
hold and continuation, with no timed credit giveaway or reacquisition. Failed
observations return to conservative reservation. Kubernetes enforces placement
between observations. Running Workflows are never preempted by this accounting.

Persisted demand bindings are validated before writing. Unreadable optional
demand metadata degrades to unknown/live accounting while the immutable Workflow
spec, UID and ownership checks remain enforced. Resource-only bindings include
executor CPU and memory and remain valid through submit/adopt round trips.

`scheduling.admission.requests.cpu_milli` shows the effective reservation. Queued
previews use effective capped requests without binding an execution target.

## Upgrade migration

The fair-share migration builds two indexes over the `runs` table in the startup
migration transaction. Even the partial active-run index must scan the table;
the started-run index includes most executed history. On a very large database,
allow maintenance time and free database/WAL/sort space on representative
storage. Increase the chart startup probe allowance if measured migration time
needs it, so repeated restarts do not interrupt the build. A small synthetic
benchmark is not evidence of production migration time.
