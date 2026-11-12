# CPU admission

Since v0.21.9 Oberth reserves each started Workflow's peak concurrent CPU
requests until it completes or cleanup proves its execution resources are gone.
Admission adds these reservations and admits a whole run only when it fits in
allocatable CPU minus headroom. CPU limits remain burst ceilings.

## Defaults

At server startup, Oberth reads Ready, schedulable nodes and uses the smallest
node's allocatable CPU as a conservative packing pool. It reserves 20% for
platform services, helpers outside Workflows, and host work. A 12-CPU node thus
has a 9600m pipeline budget. This deliberately favors single-node safety over
multi-node utilization; node affinity, taints and unrelated workloads can still
prevent placement. Restart Oberth after changing node capacity.

The server fills missing requests, then caps each step container's CPU request
at 25% of node allocatable CPU (3000m on 12 CPU). This includes scripts,
container sets, template defaults, init containers and sidecars. Limits are
unchanged: a container with request 5400m and limit 8 becomes request 3000m,
limit 8. Admission analyzes this same immutable source and fragment receipt,
including workflow parallelism and 50m executor CPU per concurrent Pod. Missing
memory limits do not disable CPU accounting. Unbounded graphs or requests
larger than the entire pool fail before Workflow creation with a remedy to
reduce requests or workflow parallelism.

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
64 prepared waiting runs so a new promotion can overtake waiting standard work.
It keeps at most one checkout preparing at a time. Smaller runs do not backfill
past the chosen larger waiter, letting enough CPU accumulate for that whole run.
Already started Workflows retain their reservations and are not preempted.

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
restored requests consume the whole pool until that Workflow finishes.
