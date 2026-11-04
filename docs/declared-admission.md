# Workflow admission and Kubernetes placement

Oberth submits workflows up to the administrator's concurrent-workflow ceiling.
Kubernetes places each step Pod using its CPU, memory, and extended-resource
requests. A Pod remains Pending when no eligible node can satisfy those requests.
Oberth does not reserve a second CPU/memory pool or pack workflows by size class.

Commit measured requests in the workflow. Explicit positive requests and limits
are preserved. Repository main containers, scripts and container-set members
receive missing requests of 2 CPU and 2Gi memory. Repository init containers and
sidecars receive smaller missing requests of 250m CPU and 256Mi memory. These
defaults also cover inline templates. Declared limits and template defaults take
precedence; limits are not added to repository containers. New server-owned
executor helpers have 50m/64Mi requests and 500m/512Mi limits; source seeding has
100m/64Mi requests and 500m/512Mi limits. Pinned controller profiles retain their
profile-bound executor resources. These limits still require live qualification.

The existing PSI hard-limit valve can hold new workflow grants under host CPU or
IO pressure. Hard pressure cannot be bypassed by release/tag priority, an idle
host, or a starvation timer. Disabling the configurable PSI valve leaves the
administrator's workflow ceiling and any configured disk-space floor in control
of admission. PSI availability is reported through the existing observation
fields; unavailable PSI retains the existing fail-open behavior.

Credential authorization, source validation, repository concurrency groups,
per-container security grants, and dedicated VM authorization/cleanup remain
separate safeguards. They do not form a global CPU/memory packing budget.

## Upgrade note

Existing installs can retain their current chart values and server arguments
while upgrading. The following settings remain accepted; their upgrade behavior
is listed below:

| Existing setting | Upgrade behavior |
| --- | --- |
| `oberth.ci/size`, `oberth.ci/size-by-trigger` | Historical annotations do not select a weight or reserve capacity. |
| `--admission-size-weights` | S/M/L/XL weights are ignored. |
| Size grants and revocations | Size grants are no longer required or enforced. Independent per-container capability/resource grants remain enforced. |
| `--admission-legacy-resources`, `admission.legacyResources` | Historical fixed per-size CPU/memory charges are accepted and ignored. |
| `--admission-cpu-millis`, `--admission-memory-bytes` | The declared CPU/memory pool is removed. Kubernetes decides whether a Pod fits. |
| `--admission-memory-headroom-bytes`, `--admission-memory-headroom-percent` | No application memory headroom reservation. |
| `--admission-extended-resources` | No application capacity ledger; Kubernetes schedules granted extended-resource requests. |
| `--disable-declared-admission` | Does not refuse workflows with declarations. |
| `--admission-io-runs` | Use the single workflow ceiling instead. |
| `--admission-max-head-wait` | Resource drain/backfill forecasts are removed. There is no resource-size head to drain for. |
| `--admission-disk-floor-bytes`, `admission.diskFloorBytes` | Retained opt-in admission hold on the data root: new workflows wait while free bytes are below the floor. Already-granted runs continue. This protects server storage; it is not a Pod capacity budget. |
| PSI soft thresholds, observation mode, max wait/defer settings | The enabled serving valve enforces only its configurable hard CPU/IO thresholds; no soft-pressure drain, settling delay, or starvation grant. |

The corresponding historical `admission.declaredResources` and size-weight chart
values are ignored by the upgraded server. Keep `maxConcurrentJobs` (and its
runtime ConfigMap override) and `admission.pressure.enabled`, `hardCpu`, and
`hardIo` configured for the install. A ceiling of 10 admits at most 10 workflows,
including granted workflows whose Pods are Pending. Raising it does not change
requests or limits on existing Pods.

A new request default is applied only when submitting a new workflow; an
already-submitted workflow is adopted without rewriting its Pods, including
workflows using the earlier uniform 2 CPU/2Gi default policy. Recovery
counts existing and uncertain workflows against the workflow ceiling until
ownership-checked cleanup is accepted or absence is confirmed.

The existing reason line reports the administrator ceiling, `waiting for disk
space`, host pressure, or recognized Kubernetes scheduling reasons such as
`Insufficient cpu` and
`Insufficient memory`. Arbitrary event text is withheld. No new run statuses or
UI layouts are introduced.

## Bounded startup and cleanup

The intentionally gated executor identity holder binds the credential lifetime
to the owned Workflow. The real step's token waiter retries issuance once after
two minutes, then fails after another two minutes without readiness. A workflow
that has never scheduled a workload Pod and has no progress fails after ten
minutes by default; `--admission-stall-timeout` configures that startup bound.
An owned Pending Pod with the scheduler's `Unschedulable: Insufficient cpu`,
`Insufficient memory`, `Insufficient ephemeral-storage`, `Too many pods`, or a
qualified extended-resource shortage suspends this clock. Kubernetes owns that
capacity wait, including waits longer than ten
minutes. Scheduling gates and other non-capacity startup stalls remain bounded.
This exemption applies to Oberth's startup clock; the workflow's declared
execution deadline still applies.
Once any workload Pod is scheduled, the workflow startup bound never restarts
between steps; retained Argo Pod-node evidence preserves that fact on recovery.
An unobserved identity waiter on an already scheduled Pod remains bounded by
the configured startup duration, measured from scheduling rather than Pod creation.

Before submission, an unavailable controller profile has its own reason:
`controller profile verification unavailable for 10m0s before workflow submission`.
Kubernetes Pod-observation or identity-read outages are reported as observation
outages after two minutes, separately from execution progress or capacity waits.

A deadline does not release a workflow slot while an unconfirmed owned workload
may still be alive. Cleanup retries in the background, preserving the fixed
failure reason. Once owned cleanup is accepted or absence is confirmed, the
slot is released; scratch-file cleanup can retry independently.
