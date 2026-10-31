# #1216: measured requests with whole-run admission

2026-10-31. Stage 1 implementation and stages 2–3 proposal. Based on the supplied
2026-10-30 queue analysis, source at v0.17.73 `ac5f7138`, and Kubernetes/Argo
behaviour. Target release: v0.17.74 (schema 36). No target operations or utilisation
change are part of this patch. The R2 protection plan pairs reduced capacity
with protection, qualifies system-critical recovery and measures cache pressure
before choosing thresholds/first limits. See docs/schema36-upgrade-note.md for
the required new release/rollback qualification.

## Ship order and invariant

**Protect the platform first:** [platform protection plan](docs/platform-protection-plan.md)
is the first independently committed deliverable. Qualify priorities, complete
platform requests, host reservations, a memory eviction threshold and disk/IO
headroom before increasing CI utilisation. Stage 1 can collect at today's caps.
Stage 2 changes requests only after coverage review. Stage 3 changes admission's
unit from size weights to measured resource vectors; it does **not** remove
admission or delegate workflow queueing to Kubernetes.

## Stage 1: observations outside the step

The existing termination-message helper remains self-reported advisory data.
It can be forged and reports nothing after a hard kill. New `MeasuredUsage`
records are produced by the Oberth server from administrator-allowlisted,
TLS-verified kubelet `/stats/summary` and `/metrics/cadvisor` endpoints. No build
endpoint, termination message, host filesystem mount or CRI socket is trusted.
There is no telemetry ingestion API, exec call or `nodes/proxy` permission.

Collection is disabled by default. The administrator enables
`--argo-resource-sampler-config=/absolute/path/config.json`, with:

```json
{
  "nodes": {"tuxbox": "https://node-serving-certificate-name:10250"},
  "ca_file": "/etc/oberth/resource-sampler/ca.crt",
  "token_file": "/var/run/secrets/kubernetes.io/serviceaccount/token",
  "interval": "15s"
}
```

The chart's `argo.resourceSampler` values create the public CA/config ConfigMap,
server mount/flag/checksum and a separate ClusterRole with `get` on
`nodes/stats`/`nodes/metrics`, restricted to the configured node names and bound
only to the server ServiceAccount. Existing frozen Argo RBAC is unchanged.
Kubelet token authentication, webhook authorization, serving certificate chain
and SANs, metrics availability, network routing and token audience must be
qualified by the operator before enabling. Failure leaves usage unknown and
never changes execution results. Do not substitute `nodes/proxy`, skip TLS
verification or give this role to CI identities. These APIs expose node-wide
telemetry to the server, so the opt-in grant is deliberately explicit.
[Kubelet authentication/authorization](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-authn-authz/).

Each running Workflow has one bounded sampling session. Immutable API
observations are handed off by pointer and copied only at sample time, rather
than at every 500ms supervision poll. The controller verifies
the run/Workflow before handing a snapshot to it. A scoped Pod LIST is checked
against namespace, Workflow controller-owner UID, node ID/name and generated pod
name. The current `main` status supplies the full runtime container ID and restart
count. cAdvisor labels must also match namespace/pod/main, and the cgroup path
must contain the exact Pod UID and full current container ID. Labels alone,
reused names, prior termination states and short container prefixes are not
authority. Different node attempts, Pod UIDs or container incarnations get
separate durable rows. A replaced Workflow UID is refused by the session.

Metric timestamps are mandatory and must be at most 30 seconds old or two seconds
ahead of the observer. `container_last_seen`/HTTP scrape time cannot establish
freshness. CPU peak is the largest interval rate of cumulative CPU time between
two fresh, increasing source timestamps at least a second apart. Memory is the
largest observed current `container_memory_usage_bytes` or the kernel high-water
`container_memory_max_usage_bytes` where exposed, including cgroup cache. The
current gauge remains the fallback when the high-water family is absent. Disk IO
uses cAdvisor's per-device service-byte/operation counters derived from cgroup IO
accounting; repeated cumulative samples are merged by maximum, never added across
polls. Pod UID-bound summary samples provide ephemeral bytes and pod IO PSI
`some`/`full` cumulative **nanoseconds**, where the runtime exposes them. Absent
counters stay null, including on systems without IO/PSI support.
[cAdvisor metrics implementation](https://github.com/google/cadvisor/blob/v0.54.1/metrics/prometheus.go),
[kubelet summary types](https://github.com/kubernetes/kubelet/blob/v0.35.7/pkg/apis/stats/v1alpha1/types.go).

CPU, memory and IO are **main-container scope**; ephemeral and PSI are **pod
scope**, including helpers, writable layers, pod logs and disk-backed emptyDirs.
Pod-wide values may span main-container restarts. They do not measure source PVC
capacity, shared caches, image-store totals, VMI guest usage or VM disks. These
scopes are explicit in the report. ContainerSets without `main`, short-lived
steps between polls, deleted pods and unavailable kubelets remain coverage gaps.
Do not apply their samples to a whole-pod or whole-VM recommendation.

Container and PSI source clocks are retained independently, so a newer cached
summary cannot hide fresh cAdvisor progress or IO counter resets.

Peaks are always **sampled lower bounds**; polling cannot prove the actual peak
between samples. Periodic writes survive a later OOM/SIGKILL without an exit
handler. Terminal observations merge into earlier records even if the final
scrape has disappeared. A killed container seen before any usable scrape gets
terminal metadata with `samples=0` and null measurements. A pod already GC'd
before observation has no record; the report therefore never claims complete
population coverage. Counter resets, overflow saturation and changed requests
are flagged. No missing field is converted to measured zero.

Bounds: 1–30 second configured polling (default 15), at most 32 configured nodes,
two shared node scrapes globally, four seconds per owned scrape, five seconds per
Workflow sampling round, at most ten seconds to join an in-flight round plus a
final sample, 16 MiB per response, 65,536 selected metrics,
32 labels and 256 KiB per metric line. One node snapshot is shared across runs
for an interval. Its lifetime is independent of the first waiter's cancellation;
credentials rotate on each request and never follow a proxy or redirect. A
Workflow round accepts at most 2,048 status nodes/Pods; larger objects are not
fully sampled. Batch writes contain at most 256 observations. Storage is capped
at 4,096 incarnations/run while existing records remain updatable. It cascades
with run deletion. The migration is appended as **v36**, preserving RCA's v35.
Provision server memory for configured node count and scrape size; do not enable
a large cluster's full-node telemetry on the supplied 1 GiB server envelope
without qualifying sampler heap/GC alongside the other platform peaks.

`run_get` attaches up to 256 records to the assembled step list, including active
steps with a plan/progress entry. `MeasuredUsageTruncated` marks that bound.
Telemetry read failure leaves the normal run view usable. Retry/rerun creates a
new run: trusted observations are not copied into its authoritative evidence.

## Stage 1 report

MCP `repo_usage` is read-only and repository-scoped before querying storage:

```json
{"name":"repo_usage","arguments":{"repo":"upstream/org/repo","since":"2026-10-01T00:00:00Z","limit":1000,"burn":"checks","step":"unit"}}
```

Omit `since` for the last 30 days by observation time; omit burn/step for all
steps in the bounded window. Indexed queries select the newest 1,000 observations
by default, maximum 5,000, with a truncation indicator; at most 128 step groups
are returned with a separate `steps_truncated` flag. `observations` counts the
selected window, including groups omitted by that second bound. Narrow burn/step
filters when truncated. This is not a repository-wide census or a random sample.

Each metric reports nearest-rank p50/p95/max and its own known-value count. CPU,
memory and ephemeral requests are reported alongside usage; used/request PPM is
computed from each paired observation with a positive request, then aggregated
(1,000,000 = 100%). Request zero is known zero but cannot be a ratio denominator.
IO/PSI have no schedulable request, so their request/ratio counts are zero. Values
include running, failed and killed container attempts, without silently selecting
only successful final attempts. Group counts expose terminal/OOM/unsampled and
flagged observations. Raw metrics, API error bodies, credentials, scripts and
container termination messages are never returned.

The report deliberately provides no automatic request recommendation. Small
sample counts, active/killed lower bounds, retries, scope changes and censored
OOM observations can bias quantiles downward. Stage 2 needs broader coverage,
representative workloads and conservative fallbacks. CPU interval peaks are not
instantaneous CPU demand; PSI totals are lifetime stall time, not a percentage.

## Stage 2 proposal: reviewable per-step requests and limits

Key training by repository, logical step, relevant pipeline/image identity and
resource scope. This first report groups only repo/burn/step; do not automatically
mix different implementations into a policy. Require representative coverage,
an explicitly chosen window and minimum count, record excluded/censored samples,
and show a proposed diff with its provenance. Unknown shapes keep authored
resources/server ceilings. **Stage 2 must not lower requests or limits from
main-container figures alone**: require independent sidecar/helper/ContainerSet,
whole-pod and VM coverage, since work can be moved outside main. Never train a smaller memory limit from OOM-censored
lower bounds or an unsampled killed step.

Propose CPU requests from a qualified upper interval quantile with headroom,
remove CPU limits only as a separately reviewed CI policy, and retain bounded
memory and ephemeral limits using conservative max/tail evidence and margin.
Include generated helpers, sidecars, restartable init overlap and pod overhead.
Treat current per-container caps as ceilings, not measured capacity. Validate the
effective emitted Pod resources against those ceilings and node/platform reserve.
Make policy version and fallback visible; allow rollback without changing history.
Resource policy is administrator-owned; authors cannot forge trusted usage or
select platform priority. IO stays a coarse whole-host ceiling/pressure hold;
bytes or PSI cannot be turned into Kubernetes scheduler requests.

## Stage 3 proposal: keep admission and change its unit

Compute a conservative **whole-run peak vector** over CPU, memory, ephemeral
requests and pod count from the resolved DAG/steps, template/workflow parallelism,
sidecars, daemons and retry/exit-handler lifetimes. Include source-seeding and
executor/token helpers plus effective init/overhead requests. Parallel siblings
add; serial phases may share reservation only when non-overlap is proven.
Unsupported/dynamic expansions must use conservative bounds or refuse the new
mode; never assume an unknown expansion is free. Couple the resolved policy and
pipeline identity to the reservation so an updated pipeline cannot consume a
stale cheaper quote. Validate sums with saturation and fail closed on overflow.

Hold the admitted peak until observed workload teardown, including cancellation
and recovery, proves its resources are gone. Admit only when held + peak fits
post-protection allocatable minus platform/other-workload reserve and uncertainty
margin. Apply the existing disk floor, IO pressure/coarse ceiling and concurrency
groups as independent gates. Reconcile durable reservations after restart before
new grants; do not release on a DB terminal flag while pods/VMs remain alive.

### KubeVirt and hold-and-wait

Preserve per-repository VMI grants and the existing token-barrier/create quota
protocol. Additionally sum launcher demand **cluster-wide across repositories**.
Include guest memory plus validated KubeVirt overhead, CPU/device requests, VM
count and VM lifetimes from create until confirmed deletion. A completed resource
`create` step does not free its VM. Account for VM disks/PVCs outside ephemeral
requests through separate disk reservations/floor and conservative IO ceilings.
VMI guest metrics require their own trusted path; stage 1 main-container usage
cannot right-size a guest.

Reserve all simultaneously live VMs and step/daemon pods as part of the run peak.
This prevents the supplied analysis's two-cell/multi-repository and daemon
hold-and-wait cycles. Per-grant limits or a count-only workflow ceiling cannot do
that. Do not add another gang scheduler to the single-node design.

### Queue, starvation and deadlines

Keep admission priority, aging, FIFO head protection, bounded backfill, cancellation
and dashboard position/ETA. Once a head reaches its maximum wait, drain fitting
capacity for it rather than allowing a permanent stream of smaller runs to win.
Report the constraining vector dimension, reserve and head/backfill state; do not
show Kubernetes' per-pod priority queue as a promised workflow FIFO position.
Use a single non-preempting CI PriorityClass; platform recovery can preempt CI.

**Keep the Workflow uncreated while it queues.** Argo's workflow deadline, template
timeout and retry maxDuration count wall time while a pod is unschedulable.
Pod activeDeadlineSeconds also includes on-node image pulls/init/token waits.
Inflating or continually patching deadlines does not implement a pause and can
extend authored execution budgets. The vector gate prevents normal capacity
queueing from consuming those clocks; unforeseen post-submission Pending time is
explicit scheduling/infrastructure evidence, not a claimed paused deadline.

Add a bounded Pending watchdog as a safety net. For multiple admitted runs in a
capacity conflict, the younger admission yields only if **no step or VM action
has executed**, checked with durable execution evidence and exact owned Workflow
and Pod/VMI identities. Cancel, confirm teardown, release reservation, then enqueue
a bounded retry under the existing automatic-retry fences. A started run is not
transparently replayable; surface the scheduling failure and preserve logs/state.
Missing evidence, quota refusal or an authored failure is not permission to replay.
Bound retries/age and retain links to the superseded attempt.

### Bound submitted work and qualify before enabling

Keep a separate hard workflow/pod-count ceiling, controller creation rate bound
and per-workflow parallelism cap so transient Pending/API outages cannot flood the
node/DB. Argo count limits are backstops, not vector admission. Stage 3 rollout is
shadow accounting first: compare predicted versus emitted effective Pod/launcher
requests, reconcile held vectors, then enable for a small allowlist with existing
weight admission still constraining it. Raise no budget until shadow errors and
platform protection tests are resolved.

Required stage 2–3 tests include DAG/steps overlap, loops/unsupported fallback,
daemon/exit-handler lifetimes, init/sidecar/overhead sums, cross-repo multi-VM cells,
concurrent grants/cancellation/recovery, stale policy/node capacity, overflow,
oldest-heavy-run starvation under small-run arrivals, bounded Pending count,
unexecuted-only watchdog retry, and dashboard queue/attempt continuity. Exercise
memory/disk/IO pressure with platform/token/DB recovery before a production canary.
Rollback restores the old gate and resource policy while retaining measurements;
increased CI utilisation cannot survive rollback of platform protection.

## Report-only size comparison follow-up

The next reviewed stage adds per-step sampled peaks grouped by the observed
Workflow S/M/L/XL admission label (or unknown), in run_get and repo_usage. It
does not implement stage2 resource policy or change stage3 admission. There
are no universal numeric CPU/memory envelopes behind these size labels.
No class is inferred from peaks or untrusted termination-message declarations.
See [report semantics and coverage](docs/measured-size-report.md).

Host PSI now defaults to observation-only. Explicit enforce mode retains the
30/20 hysteresis and bounded holds; capacity/disk/ownership gates remain
independent. The pause-Pod platform qualification does not qualify a PSI
threshold or application recovery. Do not substitute a larger unmeasured
threshold for coverage/qualification or raise capacity during a long IO hold.
