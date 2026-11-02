# Admission from reviewed declarations — Stage C

Git remains the source of truth. A workflow whose reachable pod-producing steps
all declare positive CPU and memory requests uses those scheduling requests at
peak concurrency. Its size annotation no longer determines its reservation or
requires a size grant. The administrator's per-container declaration ceilings
still apply at trusted submission. No resource value, missing limit or retry is
changed. Requests reserve capacity; an absent runtime limit remains unbounded.

Workflows with incomplete or unknown declarations retain size admission. Their
combined weight reserves the same fraction of both CPU and memory budgets when
sharing with declared work. A declared XL workflow can therefore share with
small work, while a legacy whole-budget XL keeps its existing exclusion.
Release urgency, repository groups and VM grants remain separate protections.
VM and trusted preflight workflows retain their independent admission path.

## Bounds

Static DAG and step graphs reserve the maximum concurrent CPU requests and the
maximum concurrent memory requests independently. A dependency earns ordering
credit only when it requires successful completion; early-start and OR
expressions earn no credit. Conditions are conservatively assumed to run.
Repeated calls and static array fan-outs are counted separately. Nested and inline templates, scripts,
container sets, inherited requests, authored init containers and sidecars count.
Container-set requests are summed for the pod. Init requests use a conservative
scheduling envelope, including restartable init containers.

Dynamic fan-out requires positive workflow `parallelism`, bounded to 256 pods.
Its reservation uses the largest possible declared pod in each dimension times
that bound. Unsupported lifetime graphs, hooks and exit handlers do not earn
concurrency credits. Graph expansion and analysis have explicit size/depth/work
bounds; a fully declared graph that cannot be bounded is refused before create.
Work with missing helper requests or unknown declaration coverage keeps the legacy path rather than claiming a
complete numeric reservation.

Each concurrent pod also reserves 100m CPU and 256Mi memory for server/executor
helpers. This is capacity held aside, not a mutation of pod requests. Deployment
executor defaults must fit this allowance; review them before release. CPU and
memory budgets must exclude platform and VM capacity. They are administrator
settings, not detected host capacity or measurements.

## Administrator settings

The confirmed defaults are 8 CPU, 16Gi memory, and the existing coarse run cap
for IO. One administrator kill switch sends **new** work back to size admission.
Existing declared reservations remain accounted through completion/recovery.

| Server flag | Chart value | Default |
| --- | --- | --- |
| `--admission-cpu-millis` | `admission.declaredResources.cpuMillis` | 8000 |
| `--admission-memory-bytes` | `admission.declaredResources.memoryBytes` | 17179869184 |
| `--admission-io-runs` | `admission.declaredResources.ioRuns` | 0, retain existing cap |
| `--disable-declared-admission` | `admission.declaredResources.disabled` | false |

IO remains independent of CPU/memory fit. The coarse run ceiling, disk-space
floor and existing host-pressure gate remain in force. A positive IO-run cap can
only narrow concurrency. The existing queue, starvation/backfill window,
repository groups, cancellation and uncertain-create ownership are retained.
Oversized numeric demand is refused instead of waiting forever for a budget it
cannot fit. Admission messages expose this run's declared demand without adding
run-row labels or exposing another repository's capacity usage.

The same immutable source and fragment receipt supplies reservation and trusted
submission. Submission refuses a changed resource graph. A server-owned numeric
binding is included in workflow identity and restart adoption; branches cannot
forge it. Uncertain creates persist the bound demand in the existing local
admission journal before create. Recovery never re-sizes running work from a
moving branch or fragment tag. No database schema change is required.

Platform protection is enabled by default using the existing high-priority
classes and deployment hooks. Pipeline-authored priorities and pod patches
remain refused. External platform workloads still require the existing
installation handoff to use their retained recovery classes; validate that
placement outside this laptop before rollout.

After release, run branch CI and inspect, for example:

```
oberth run resources --repo repo-01 --ref any --last 100 --include-failed --yaml
```

Review complete, identity-bound history, then commit declarations in the owning
workflow with the code change. This is per repository, reviewed and revertible.
The anonymised table cannot supply suggestions for ambiguous attempt identity.
There are no per-repository server modes or automatic sizing changes.

Stage D remains deferred: existing workflow deadlines include Pending time,
and a bounded pending population, starvation policy and queue/ETA semantics
have not been established. Stage C keeps the current server-owned queue.
