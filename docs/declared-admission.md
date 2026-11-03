# Admission from reviewed declarations

Git owns resource values. Oberth never derives admission demand directly from
observed usage or modifies a workflow's requests or limits. Commit reviewed
measurements from `oberth run resources` in the workflow.

An ordinary workflow with CPU/memory declarations uses numeric admission. Every container
must declare positive CPU and memory requests and a positive memory limit,
directly or through template defaults. This includes scripts, container sets,
init containers and sidecars. CPU limits are optional. Its S/M/L/XL annotation
does not determine the reservation or require a size grant.

Partial declarations, unsupported graphs without a provable bound, and demand
that exceeds the configured budget are refused before submission. The error
identifies the missing declaration or budget and directs the operator to change
git declarations/parallelism or administrator settings. They never use the
legacy size fallback. An administrator disabling declared admission refuses
new declared runs; it does not convert them to size admission.

Undeclared legacy workflows retain size admission, with a deprecation warning
in `resource_message` directing the repository to commit measured
requests/limits in git. Their combined weight reserves the same fraction of
both host budgets when sharing with declared work. A legacy whole-budget XL
retains its existing exclusion. The warning survives submission and restart
through immutable timing metadata; `display_reason` keeps its existing meaning.

## Resource accounting

The graph reserves independent upper bounds on concurrent CPU and memory
requests. Memory limits remain Kubernetes runtime ceilings. Kubernetes schedules
pods by their actual requests and queues Pending pods when capacity is unavailable.
The administrator's per-container numeric request/limit ceilings also apply at
trusted submission and release preflight; see [declared resources](declared-resources.md).

Static DAG and step graphs account for independent calls, dependency ordering,
nested and inline templates, container sets and authored helpers. Init demand
uses a conservative scheduling envelope, including restartable init containers.
Conditions are assumed to run; early-start and OR dependencies earn no ordering
credit. Mutable item counts earn no reductions beyond enforced parallelism.

Workflow `parallelism` caps concurrent workflow pods. A steps/DAG template limit
caps its concurrent child invocations within that invocation. Nested limits
retain each child's full envelope. Dynamic loops require an applicable bound.
Unsupported graphs with a workflow bound reserve the largest possible pod per
slot. Unbounded sequences, hooks and exit handlers without a supported bound
are refused. Analysis has explicit size, depth and work bounds.

Each peak concurrent executor pod adds 100m CPU and 256Mi memory. Each declared
run also reserves the larger of a fixed minimum (default 1Gi) and a percentage
(default 10%) of the concurrent memory request envelope. The percentage rounds
up to whole bytes before executor reserves. These are administrator-owned
allowances, not pod mutations or usage estimates. Budgets must exclude platform
reserve and external workloads.

For example, two runs each requesting 20Gi with one executor pod reserve
2 × (20Gi + max(1Gi, 2Gi) + 256Mi) = 44.5Gi and fit a 48Gi budget. Their memory
limits may be higher. This is a coarse packing margin; it does not guarantee
that all containers can simultaneously use their full limits.

Trusted contract runs retain size admission, even with declarations. Their
reservation uses the larger main-document and trusted-preflight size, including
the server leaf minimum, and requires the corresponding size grant. Capacity
retries retain that reservation. Numeric admission for these runs must wait
until the preflight and main-document demand are both declared and charged.

## Queue and IO controls

IO is not inferred from CPU or memory declarations. The coarse run cap,
disk-space floor and host IO-pressure gate remain in force for declared and
legacy work. A positive IO run cap can only narrow concurrency. Existing queue
priority, FIFO ordering within priority, bounded backfill, repository groups,
cancellation and uncertain-create ownership remain in force.

| Server flag | Chart value | Default |
| --- | --- | --- |
| `--admission-cpu-millis` | `admission.declaredResources.cpuMillis` | 8000 |
| `--admission-memory-bytes` | `admission.declaredResources.memoryBytes` | 17179869184 |
| `--admission-memory-headroom-bytes` | `admission.declaredResources.memoryHeadroomBytes` | 1073741824 |
| `--admission-memory-headroom-percent` | `admission.declaredResources.memoryHeadroomPercent` | 10 |
| `--admission-io-runs` | `admission.declaredResources.ioRuns` | 0, retain existing run cap |
| `--disable-declared-admission` | `admission.declaredResources.disabled` | false |

Unset headroom settings keep their defaults independently of custom budgets;
explicit zero disables that component. Both must be zero to disable headroom.

## Upgrade and recovery limits

Migration 48 copies historical size grants into numeric per-container ceilings,
including grant/revocation provenance. Fresh numeric grants are independent of
legacy size and tag-priority policy. Existing reservations restore their sealed
demand without recalculating it from current git, even with declared admission
disabled; helper and headroom reserves use the restoring server's settings.
Submission verifies the immutable source and resolved fragments against the
reservation before creating a Workflow.

Review older low requests paired with high limits before relying on increased
concurrency. Representative measurements, actual scheduler overlap, Pending
handling/deadlines, queue fairness under load and platform pressure survival
still need live acceptance. This change neither creates measurements nor
changes Kubernetes or Argo deadline behavior.
