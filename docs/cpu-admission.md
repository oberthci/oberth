# CPU admission

Since v0.21.10 Oberth reserves each started Workflow's peak concurrent CPU
requests through execution and cleanup, with bounded credit leases for unknown
restored work and parked assistance.
Admission adds these reservations and admits a whole run only when it fits in
allocatable CPU minus headroom. CPU limits remain burst ceilings.

## Defaults

At server startup, Oberth sums allocatable CPU across Ready, schedulable nodes
without NoSchedule or NoExecute taints. It reserves 20% for platform services,
helpers outside Workflows, and host work. A 12-CPU node thus has a 9600m
pipeline budget. Per-step caps use the largest eligible node. Each remote
execution target is sized from its own authenticated cluster, with independent
admission credits. Affinity and unrelated workloads can still prevent Pod
placement. Restart Oberth after changing node capacity.

The server fills missing requests, then caps each step container's CPU request
at 25% of the largest eligible node’s allocatable CPU (3000m on 12 CPU). This
includes scripts,
container sets, template defaults, init containers and sidecars. Limits are
unchanged: a container with request 5400m and limit 8 becomes request 3000m,
limit 8. Admission analyzes this same immutable source and fragment receipt,
including workflow parallelism and 50m executor CPU per concurrent Pod. Missing
memory limits do not disable CPU accounting. Unknown graphs (including hooks,
exit handlers and unbounded withParam) reserve
the whole pool. Wider fan-out is clamped to the pool for admission. Both remain
valid workflows; Kubernetes placement and the Insufficient-CPU timeout bound
the execution wait. These sizing cases do not consume infrastructure retries.

When at least five distinct successful runs have complete CPU coverage for a
template, Oberth sizes its main container from the P95 of CPU time divided by
container lifetime, adds 25%, rounds up to 10m, and applies a 50m floor. One run
gets one vote (the largest invocation average); instantaneous peaks do not set
requests. History is isolated by repository and execution class and bounded to
the latest 128 started runs and 4096 usage rows. Incomplete, reset, restarted or
saturated samples are excluded. Missing history keeps capped declarations.
Sizing only lowers requests and never changes limits. The chosen per-template
policy is frozen before admission and retained on the Workflow for retries.

`admission.cpuEnabled: false` or `--admission-cpu-enabled=false` disables CPU
admission, the node-capacity read, request caps and history-based resizing for
local and remote targets. Other admission gates still apply.

The chart values `admission.cpuHeadroomPercent` and
`admission.stepCPURequestPercent` correspond to serve flags
`--admission-cpu-headroom-percent` and `--step-cpu-request-percent`.
Pressure uses `admission.pressure.hardCpu` (25), `hardIo` (40) and
`deferSeconds` (300), exposed as `--admission-pressure-hard-cpu`,
`--admission-pressure-hard-io` and `--admission-pressure-defer-seconds`.

The server needs `get,list` on nodes, without node proxy or write privileges.
Unavailable allocatable CPU stops startup. Unavailable PSI leaves CPU packing
active. The pressure gate defers a fitting run for up to 300 seconds and then
admits one; this never bypasses the CPU budget, disk floor or administrator hold.

## Ordering and bounded waits

Priority remains `low < standard < promotion < high`. Within one class,
repositories with fewer active runs go first; the least recently served
repository breaks ties, then FIFO applies. Admission looks ahead through up to
8 prepared waiting runs so a new promotion can overtake waiting standard work.
It keeps at most one checkout preparing at a time. A capacity-held head alternates
one-minute drain and backfill windows, allowing smaller work through while
periodically accumulating room for large work. A blocked cluster does not hold
other clusters. Assisted attempts parked for a minute stop consuming admission
credits until continued; resource ownership and Kubernetes placement remain
intact. Started Workflows are not preempted.

Argo receives `spec.priority` -1 for low, 0 for standard (omitted), 1 for
promotion and release, and 2 for high. Repository YAML cannot set it. Fresh
managed Argo installs still derive parallelism from allocatable CPU (half the
cluster's cores, at least two); CPU packing provides the capacity bound even
when Argo's limit is disabled. An existing Argo limit remains an additional cap.

An owned Pod continuously reporting `Insufficient cpu` expires after
`admission.stallTimeout` (10 minutes by default), including a later step after
earlier steps ran. The timer uses Kubernetes timestamps and survives observer
restarts. The existing infrastructure failure, cleanup and retry paths apply;
capacity is retained while deletion is uncertain. Native Argo lock waits remain
exempt. This bounds the observed CPU wait plus the observation interval;
unavailable Kubernetes cleanup can extend how long resources remain reserved.

`scheduling.admission.requests.cpu_milli` shows the effective reservation.
Restarts restore the bound reservation from the submitted Workflow; unknown
restored requests consume the whole pool for a one-minute credit lease, dated
from the durable start time so restarts do not renew it. A failed demand read
degrades to unknown and does not abort startup. Ownership remains until cleanup.
The queued run list shows capped effective CPU requests (including executor
allowances); unknown graphs remain unknown until execution supplies observations.
