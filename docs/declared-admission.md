# Admission from reviewed declarations — Stage C

Git remains the source of truth. A workflow whose reachable pod-producing steps
all declare positive CPU and memory requests can use numeric admission at peak
concurrency only when every container also has a positive memory limit, directly
or inherited. This includes scripts, container-set members, init containers and
sidecars. CPU and ordinary-container memory reserve their requests, including
inherited requests. Memory limits remain Kubernetes runtime ceilings. On that
numeric path, its size annotation no longer determines its reservation or
requires a size grant. The administrator's per-container declaration ceilings
still apply at trusted submission. No resource value, missing limit or retry is
changed. An absent runtime limit remains unbounded and keeps legacy admission.

Workflows with incomplete or unknown declarations retain size admission. Their
combined weight reserves the same fraction of both CPU and memory budgets when
sharing with declared work. A declared XL workflow can therefore share with
small work, while a legacy whole-budget XL keeps its existing exclusion.
Release urgency, repository groups and VM grants remain separate protections.
VM workflows participate in numeric host admission when their declarations and
graph can be bounded and fit. Separate VM cell quotas and trusted preflight
eligibility remain enforced.

## Bounds

Static DAG and step graphs reserve the maximum concurrent CPU requests and the
maximum concurrent memory reservations independently. A dependency earns ordering
credit only when it requires successful completion; early-start and OR
expressions earn no credit. Conditions are conservatively assumed to run.
Repeated calls and static array fan-outs are counted separately. Nested and inline templates, scripts,
container sets, inherited requests, authored init containers and sidecars count.
Container-set requests are summed for the pod. Init requests use a conservative
scheduling envelope, including restartable init containers.

Workflow `parallelism` caps the total concurrent workflow pods. A positive
`parallelism` on a steps or DAG template caps its concurrent **child
invocations**, shared across sibling tasks and loops in that invocation. Template
defaults apply when the template does not override the limit. Static `withItems`,
array `withParam`, and literal `withSequence` loops reserve at most the smaller
of their item count and the applicable limit. A limit on a container/script leaf
does not serialize separate invocations of that leaf.

Nested limits apply at their own boundaries. An outer limit of 2 invoking an
inner four-pod fan-out can reserve 8 pods; it cannot safely be treated as a
workflow-wide two-pod cap. The outer limit caps the number of inner invocations,
and each retains its full inner demand. Repeated and inline calls follow the
same rule. CPU, memory and pod count retain independent conservative bounds;
when child costs differ, the largest permitted child demands are charged in
each dimension. Dependency ordering can reduce that envelope.

Dynamic `withParam` or `withSequence` loops use the full applicable template or
workflow limit because their item count is unknown. An outer template limit
alone cannot bound an unbounded dynamic fan-out inside a child template.
Workflow limits are bounded to 256 pods by the analyzer; the existing trusted
submission ceilings still apply. Unsupported graphs with a workflow bound
reserve the largest possible declared pod in each dimension times that bound.
Unsupported lifetime graphs, hooks and exit handlers do not earn concurrency
credits. Nonempty array fan-outs and dependency graphs without parallelism retain
their previous resource accounting; sequences without a limit retain legacy
fallback. Empty `withItems` follows the executor's single-call behavior.
Conflicting loop sources use the full enforced bound, or legacy admission if
there is no bound, so an ambiguous smaller count cannot reduce the reservation. Graph expansion and analysis have explicit size/depth/work
bounds. A graph that cannot be bounded falls back to legacy size admission;
so does a reservation that exceeds the configured numeric budget, including
helper and run headroom reserves. These cases never acquire a new numeric
sizing refusal. A fixed notice appears in the existing run-message cell through `resource_message`,
without changing `display_reason`. Numeric admission shows the effective pod
concurrency used as an upper bound. The notice explains full-bound dynamic
fan-out, nested child envelopes, ignored conditions/early-start dependencies,
and largest-pod graph fallbacks when applicable. It is available while waiting, retained in
submission timing metadata, and restored after uncertain creation/restart.
Work with missing helper requests or unknown coverage also keeps the legacy path.
Requests are checked before limits: work without CPU/memory requests receives a
missing-requests notice, rather than a missing-memory-limit notice.
Missing or nonpositive memory limits use the same legacy path with a fixed
memory-limit notice in the run message. No memory or CPU limits are manufactured.
Existing source validity, grants and per-container ceilings still apply.

Each concurrent pod also reserves 100m CPU and 256Mi memory for server/executor
helpers. Each declared run also holds max(fixed bytes, a percentage of its
concurrent memory request envelope), defaulting to max(1Gi, 10%), for usage above
requests. The percentage is rounded up to whole bytes before executor reserves.
This is capacity held aside, not a mutation of pod requests. Deployment executor
defaults must fit this allowance; review them before release. CPU and
memory budgets must exclude platform reserve and other external work. Declared
VM host demand is included in these budgets. They are administrator settings,
not detected host capacity or measurements.

## VM host demand and memory policy

A VM reserves its declared CPU request, or its CPU limit when no request exists.
VMI memory uses its positive declared request as a bound when no limit exists,
or its limit when no request exists; with both, it reserves the higher value.
The admitted VMI schema refuses guest-memory overrides and unknown fields, so
a request-only VMI cannot raise guest memory independently of that request.
Ordinary containers in VM workflows and init containers/sidecars on their resource
templates follow the same memory-limit rule, including inherited helpers.
Each VM adds a separate
100m CPU / 256Mi launcher reserve; workflow executor pods retain their own
100m/256Mi reserve. The existing run-aware create/delete lifetime proof computes
VM peaks in these host-cost units. If that proof is unavailable, the conservative
sum is used. Workflow pod peaks are added conservatively; simple bounded VM exit
cleanup also counts. The same pinned source/fragment receipt is verified before
create. VM cell quotas still count guest cores and the higher declared memory
value, so sharing host capacity cannot increase a cell grant.

A VM with no scheduling requests or limits earns no numeric host credit and
keeps legacy sizing with a notice. Its cell reservation conservatively uses the
per-instance memory ceiling and declared guest cores. Partial invalid declarations
still fail validation; guest-core bounds, image/identity policy and cell grants
remain enforced. No request or limit is inserted into the manifest.

Ordinary-container requests should represent reviewed, measured maximum peaks.
Kubernetes queues pods by requests and enforces limits; the host admission budget
and run headroom provide a coarse packing ceiling rather than reserving
every possible limit simultaneously. Proportional headroom scales with large
runs while the fixed minimum covers small ones. Two 20Gi-request / 30Gi-limit
runs, each with one executor pod, reserve 2 × (20Gi + max(1Gi, 2Gi) + 256Mi) =
44.5Gi and fit a 48Gi budget. Administrators should tune both headroom settings
to observed overshoot and platform capacity. VM guest accounting continues to
reserve its conservative request/limit envelope; its combined host demand also
receives run headroom. Review existing memory requests that were never measured
before relying on the new admission behavior.
A request-only pod container cannot earn numeric host credit: it falls back
to legacy admission with a visible notice. Review and commit the missing limits
in git to become eligible; Oberth does not fill them in. Existing running
reservations restore their sealed demand rather than recomputing it from git;
helper and run headroom reserves use the restoring server's settings.
Both helper allowances must be checked against deployment defaults before rollout.
VM workflows that exceed the default 8 CPU / 16Gi budget retain legacy admission;
sharing those runs requires an administrator to raise the host budget to fit
their declarations plus helper/launcher and run headroom reserves. This is an
operator rollout step.

## Administrator settings

The defaults are 8 CPU, 16Gi memory, max(1Gi, 10%) headroom per declared run, and
the existing coarse run cap for IO. One administrator kill switch sends **new**
work back to size admission.
Existing declared reservations remain accounted through completion/recovery.

| Server flag | Chart value | Default |
| --- | --- | --- |
| `--admission-cpu-millis` | `admission.declaredResources.cpuMillis` | 8000 |
| `--admission-memory-bytes` | `admission.declaredResources.memoryBytes` | 17179869184 |
| `--admission-memory-headroom-bytes` | `admission.declaredResources.memoryHeadroomBytes` | 1073741824 (1Gi); 0 disables fixed minimum |
| `--admission-memory-headroom-percent` | `admission.declaredResources.memoryHeadroomPercent` | 10 (integer 0..100); 0 disables proportional component |
| `--admission-io-runs` | `admission.declaredResources.ioRuns` | 0, retain existing cap |
| `--disable-declared-admission` | `admission.declaredResources.disabled` | false |

Unset headroom settings use defaults independently of CPU/memory budgets;
explicit zero is preserved. Both components must be zero to disable headroom.

IO remains independent of CPU/memory fit. The coarse run ceiling, disk-space
floor and existing host-pressure gate remain in force. A positive IO-run cap can
only narrow concurrency. The existing queue, starvation/backfill window,
repository groups, cancellation and uncertain-create ownership are retained.
Oversized numeric demand falls back to its existing size class, with a run
message explaining the choice. It neither waits forever for impossible numeric
capacity nor refuses an otherwise valid pipeline merely for exceeding that
budget. Admission status messages expose this run's total reservation, including
helper and memory headroom reserves, without adding run-row labels or exposing another repository's capacity usage.

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
