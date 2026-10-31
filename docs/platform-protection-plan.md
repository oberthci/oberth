# #1216: platform protection plan R2 — v0.17.74

2026-10-31. Replaces the first plan after `REVIEW-MEASURED-REQUESTS.md`.
Source base: final v0.17.73 `ac5f7138`; target: **v0.17.74, schema 36**. This is a
reviewable operator plan, not deployed configuration. No worker accessed tuxbox.
Baseline numbers come from the coordinator's 2026-10-30 queue analysis.
R2 review nits incorporate the coordinator's decision: **positive XL weight 4**,
continued eBPF releases, and persistent capacity values for git-tag Helm upgrades.

**Capacity drops with protection, not later.** Start with **2 M jobs** through
#1215, down from the supplied live 4; qualify 3 only after measuring the emitted
workload. The previous CPU envelopes would leave at most 7.25 CPU for CI, about
40% below the old 12-CPU allocatable total. Expect lower parallelism/throughput.
Protecting the platform without reducing intake in the same maintenance change
would increase Pending time and spend workflow deadlines. Do not do that rollout.

## 1. Measure before setting first limits or thresholds

Collect at least one representative heavy CI day (24 hours minimum, longer if it
misses releases/VM cells) before choosing the platform envelopes or memory
thresholds. Capture 15-second kubelet samples, startup/unseal/DB recovery peaks,
controller reconciliation latency, CPU usage and throttling, per-container memory
current/high-water where available, log/tmp/emptyDir peaks, pod restart/OOM and
kubelet eviction events. `kubectl top` can supplement this, but its working-set
snapshot is not a memory peak. The step sampler observes workflow main containers;
it is **not** already a platform or VMI guest sampler. Use a separately scoped,
read-only node/platform observation for Argo/OpenBao and every helper container.
Include the actual current **eBPF release/kernel-matrix requests** on this
measurement day: effective app/init/sidecar/overhead requests, parallel pod
lifetimes and simultaneously live VM/launcher demand. Do not infer them from
the XL label, an old 8-CPU example or main-container usage alone. Preserve eBPF
release availability; revise the proposed reserve/resource envelopes against
these measurements before rollout rather than planning to suspend releases.

For the node, retain kubelet `memory.available` over that same day, host
`MemAvailable`, active/inactive file cache, PSI, node pressure conditions/taints,
free bytes/inodes and host/k3s consumption. Kubelet treats active file cache as
used; a cache-heavy build can therefore cross its threshold even while the host
can reclaim memory. Soft thresholds also report pressure/taints before their
eviction grace elapses. Grace alone does not make Pending deadlines safe.
[Cache accounting and eviction](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/).

Argo and OpenBao are BestEffort in the supplied baseline. Do not apply their first
CPU/memory limits from the old table. Set measured requests first, keeping their
existing limits unchanged; this is an intermediate Burstable state, not a claim
of kernel-OOM protection. Only after recovery/load qualification set equal CPU
and memory request/limit on **every** platform app, init and sidecar container,
then verify actual Guaranteed QoS. Include all replicas and pod overhead.
Qualify CPU throttling and latency before adding a CPU limit: a 1-CPU limit is a
new controller/OpenBao throttle and may reduce a recent Go runtime's GOMAXPROCS.
Keep or raise existing approved limits; do not blindly replace them with a table.

Choose memory envelopes from observed startup/recovery/load maxima plus at least
25% headroom and explicit uncertainty for unobserved cases; OOM-censored samples
are not evidence for a smaller limit. OpenBao may return sealed after restart:
its rollout requires an operator-owned unseal/auto-unseal verification, not just
Pod Running. Do not record keys/tokens or put them in command arguments/reports.

Set measured ephemeral **requests first**. Add new ephemeral limits/emptyDir caps
only after observing log/tmp peaks, rotation and recovery headroom. A storage
limit is a separate loss-of-service path regardless of ordinary priority; it
must not be justified as protection just because a request was justified.
Oberth already has limits; its chart derives `/tmp` sizeLimit from the ephemeral
limit, so changing one also changes the other. Keep that coupling explicit and
keep existing limits until measured. PVCs, shared caches, image storage and VM
disks need separate disk accounting/rotation/backup policy.

**Critical pods are an exception to eviction-based storage enforcement.** In
Kubernetes 1.36.5, disk-backed emptyDir, pod and container ephemeral-limit checks
all call `evictPod`, which returns false for critical pods. Once promoted, these
four cannot rely on `sizeLimit` or ephemeral limits to stop a leak by eviction;
Oberth's disk-backed `/tmp` is included. Qualify actual filesystem/project quotas
where used, bounded application writes, log rotation, monitoring and operator
reclamation independently. Memory-backed tmpfs has separate mount/memory
behaviour; this statement is about eviction, not a universal filesystem quota
exception. A platform leak can instead consume CI's disk headroom.
[Storage checks and critical-pod guard](https://github.com/kubernetes/kubernetes/blob/v1.36.5/pkg/kubelet/eviction/eviction_manager.go).

## 2. Lower CI capacity in the same change: numbers and #1215

The old envelope is an **illustration of cost**, not a deployment recommendation:

| Item | CPU |
| --- | ---: |
| Supplied old allocatable | 12.00 |
| Proposed host system reservation (qualify first) | -1.00 |
| Proposed kube reservation (qualify first) | -0.50 |
| Illustrative post-reservation allocatable | 10.50 |
| Old four main-container envelopes: 1 + 1 + 1 + 0.25 | -3.25 |
| Remaining upper bound for CI | **7.25** |

This still excludes other controllers, helpers, replicas, overhead and uncertainty;
the actual CI budget is smaller. Measured requests may change these numbers.
Read post-restart allocatable and inventory **all** non-CI effective pod requests;
do not subtract host/eviction reserves twice. The supplied 11.535/12 CPU requests
already include non-CI usage and are not a per-workflow peak measurement.

Initial configuration proposal, alongside the measured platform changes:

```yaml
maxConcurrentJobs: 2
admission:
  runtimeCapacity:
    enabled: true
    maxConcurrentJobs: 2
  sizeWeights:
    S: 1
    M: 2
    L: 3
    XL: 4
```

M=2 means a **weight budget of 4**, versus old jobs=4/budget=8. Two M jobs fit
that weight budget; it is not proof that their CPU/memory/VM demands fit 7.25 CPU.
Start the canary at one representative workflow, then two only if the combined
emitted pod/launcher peak fits measured CI headroom. A third M job would mean
budget=6, permitted only after the same proof. With XL=4, one XL fills an initial
budget of 4 arithmetically; it has no whole-budget exclusion flag. At budget=6,
XL=4 can share with M=2, subject to measured resource fit and the other gates.
Do not restore exclusive XL=0 as a compatibility step. If measured eBPF release
requests do not fit the illustrative 7.25-CPU pool, revise the proposed platform /
host reservations before applying them; **do not plan to suspend eBPF releases**.
Do not hide a mismatch by extending deadlines, raising a weight budget without
resource proof or lowering requests from incomplete main-container usage.

**Existing live `XL=8` is a compatibility trap.** Runtime capacity validation
requires a positive XL weight to fit `jobs × M`: it will reject jobs=2/3 with
XL=8 and retain the old capacity. First arrange the drained Helm/server rollout
with **XL=4**, boot jobs=2 and runtime jobs=2 together. Positive XL=4 fits both
the initial budget of 4 and a later budget of 6 without exclusive semantics.
Do not change default size weights in code or the release's general defaults.
If XL is already 4 and the mounted #1215 source is working, later adjustments
can be hot ConfigMap edits. For the example release/name only:

```sh
kubectl -n oberth patch configmap oberth-admission --type=merge -p '{"data":{"max-concurrent-jobs":"2"}}'
```

**Persist the same change in `~/.config/oberth/capacity-values.yaml`.** Merge the
boot/runtime jobs=2 and S=1/M=2/L=3/XL=4 values above into that authoritative file,
preserving its other overrides, in the same operator change as the live update.
A ConfigMap-only patch is transient: the next upgrade can restore jobs=4 and old
XL from the saved file. Save its prior contents for rollback too.

**Tuxbox upgrades use Helm with the git-tag chart**, not `oberth upgrade`.
Render and apply the exact reviewed tag's chart with the actual ordered values
files, including `capacity-values.yaml`; ensure no later file/`--set` restores
old values. Inspect the rendered Deployment's boot jobs/size weights and the
runtime ConfigMap together, then verify the installed effective configuration.
This persistent file matters to the git-tag Helm path as well as CLI upgrades.

Resolve actual release/namespace first. Never create a guessed second ConfigMap.
The source is polled every 5 seconds after Kubernetes projects the update; this
can take longer than 5 seconds. Verify `system_status` admission configuration
reports `source=file`, `max_concurrent_jobs=2`, weight capacity=4, no reload error,
and the `admission.capacity.change` audit. A ConfigMap edit/command success alone
is not acceptance. A failed read/audit retains last-good capacity. Boot jobs=2 is
the safe restart fallback. Lowering does not revoke existing grants: pause intake
and fully drain before reducing allocatable. Keep the disk floor (qualified
160 GB starting point). The default `admission.pressure.mode=observe` provides
no IO pressure hold. Enable a hold only with explicit
`admission.pressure.mode=enforce` after qualifying the threshold and capacity
together. Its per-run maxDefer bypass does not reserve CPU/memory and is not
the safety gate.

## 3. Pressure recovery needs both tolerations and a critical platform class

For this dedicated single-node CI platform, the proposed class for **only**
Oberth, workflow-controller, OpenBao and cloudflared is the existing built-in
`system-cluster-critical`. They are recovery dependencies of token delivery,
workflow reconciliation, durable state or operator ingress. Leave
`system-node-critical` to node-essential components; do not edit/recreate either
built-in class or move application namespaces into kube-system. Inventory and
reserve CoreDNS/CNI/KubeVirt control components too; their recovery must not be
sacrificed to make these four fit.

**Priority ordering has a KubeVirt consequence:** the upstream v1.9.1 operator
manifest defines `kubevirt-cluster-critical` as **1,000,000,000**, and its control
pod templates use that class. Proposed platform priority **2,000,000,000**
outranks it and can therefore preempt KubeVirt controls if CI reclamation is not
sufficient. Do not promise that only CI can be a victim. Record actual installed
class values, policies and control-pod priorities during measurement/qualification;
include their resource/recovery headroom and rehearse the failure case before
rollout. Do not silently modify KubeVirt's class to fix the ordering.
[KubeVirt v1.9.1 class](https://github.com/kubevirt/kubevirt/blob/v1.9.1/manifests/release/kubevirt-operator.yaml.in),
[control pod templates](https://github.com/kubevirt/kubevirt/blob/v1.9.1/pkg/virt-operator/resource/generate/components/deployments.go).

Why change the first plan's custom 1,000,000 class: Kubernetes 1.36.5's kubelet
`eviction.manager.Admit` admits critical pods during disk pressure, while an
ordinary high-priority pod can be rejected even after the scheduler places it.
`IsCriticalPod` requires priority >=2,000,000,000 (or static/mirror status).
A custom user class cannot be raised into that reserved range.
[Kubelet admission](https://github.com/kubernetes/kubernetes/blob/v1.36.5/pkg/kubelet/eviction/eviction_manager.go),
[critical predicate](https://github.com/kubernetes/kubernetes/blob/v1.36.5/pkg/kubelet/types/pod_update.go),
[priority validation](https://github.com/kubernetes/kubernetes/blob/v1.36.5/pkg/apis/scheduling/validation/validation.go).

Add the following to all four **pod templates**, without replacing their
existing tolerations, measured resources or security settings:

```yaml
priorityClassName: system-cluster-critical
# Resolve the built-in class; never hand-author spec.priority.
tolerations:
  - key: node.kubernetes.io/memory-pressure
    operator: Exists
    effect: NoSchedule
  - key: node.kubernetes.io/disk-pressure
    operator: Exists
    effect: NoSchedule
```

Tolerations pass the scheduler's taint filter; they do not reserve resources,
clear pressure, exempt a pod from every failure or make custom priority critical.
Do not add blanket tolerations to CI. Non-BestEffort pods can already receive a
memory-pressure toleration automatically; specify the two intended platform
keys explicitly so the recovery policy is reviewable.

**Namespace/policy qualification is mandatory before restart.** The upstream
1.36.5 Priority admission code has no kube-system-only check, but ResourceQuota
limitedResources, scoped quotas or admission policy can restrict critical-class
consumption outside kube-system. Inspect the actual policy/quotas in each of the
four namespaces and server-dry-run the actual rendered replacement Pod in each.
If a scoped quota is required, provision a bounded allowance sized for measured
replicas and rollout overlap in the platform namespace only, then repeat the
dry-run. Do not assume a managed-cluster default applies to k3s, bypass a denied
policy, or silently fall back to the inadequate 1,000,000 class.
[Priority admission source](https://github.com/kubernetes/kubernetes/blob/v1.36.5/plugin/pkg/admission/priority/admission.go),
[priority-class quota policy](https://kubernetes.io/docs/concepts/policy/resource-quotas/#limit-priority-class-consumption-by-default).

Keep the one ordinary CI class `oberth-ci`, value=0, globalDefault=false,
preemptionPolicy=Never. Releases retain Oberth admission priority/aging rather
than preempting branch steps. Platform recovery may preempt lower-priority CI;
only exact owned preemption/eviction evidence can label that infrastructure, and
started steps are not transparently replayable. Preserve authored placement/class
rejection and enforce it for VM launcher/resource/helper paths. RBAC cannot by
itself restrict a pod's class field; audit any principal able to create platform
Pods and restrict that path with admission policy where needed. Neither namespace
quota nor an operator label alone is a ServiceAccount allowlist.

## 4. Memory thresholds: measure cache behaviour, then qualify a lower hard floor

Withdraw the original unconditional hard 2 GiB proposal. After section 1, a
**candidate for the isolated pressure rehearsal** is:

```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
systemReserved:
  cpu: "1"
  memory: 2Gi
kubeReserved:
  cpu: 500m
  memory: 1Gi
evictionHard:
  memory.available: 1Gi
  imagefs.available: "5%"
  nodefs.available: "5%"
evictionSoft:
  memory.available: 2Gi
evictionSoftGracePeriod:
  memory.available: 2m
evictionMaxPodGracePeriod: 30
evictionPressureTransitionPeriod: 1m
evictionMinimumReclaim:
  memory.available: 256Mi
```

These are starting hypotheses, not approved host values. Reject the 2 GiB soft
value if the heavy-day trace routinely crosses it with reclaimable cache and low
PSI; qualify lower values or reduce cache/CI pressure before enabling. Hard 1 GiB
may still be too high for page-cache accounting or too low for rapid anonymous
allocation; choose from measured troughs/rate-of-rise and the OOM/eviction tests.
Do not remove every memory backstop merely to avoid taints. A 1-minute transition
reduces stale-pressure blocking compared with the default 5 minutes but can
oscillate; qualify recovery and taint flapping explicitly. Neither grace nor
shorter transition pauses workflow deadlines.

Preserve all existing effective disk/inode/PID thresholds: supplying a partial
map must not accidentally drop them. Match reservation accounting to measured
host/k3s processes once, not in both system and kube reservations. Do not enforce
reserved cgroup paths before configuring and qualifying them. Use an
operator-owned later-sorted k3s drop-in such as
`/var/lib/rancher/k3s/agent/etc/kubelet.conf.d/20-oberth-protection.conf`, preserve
its previous presence/contents/ownership and all CLI overrides, and do not edit
`00-k3s-defaults.conf`. Verify merged configz and new allocatable after restart.

## 5. Ordered rollout, observations and stop conditions

1. **Preflight day:** finish measurements, settle resource/threshold candidates,
   dry-run namespace/class policy and pass the isolated recreation tests below.
   Save release chart/image/value pins, workloads, runtime ConfigMap, effective
   flags, `capacity-values.yaml` contents and values-file ordering, kubelet
   CLI/config/drop-in state (including absent file), node capacity,
   DB/OpenBao backups and unseal recovery method. Keep independent host access;
   cloudflared rollout can cut the very tunnel used to recover it.
2. **Pause and completely drain:** no new submissions; wait for all Workflow,
   helper, source-seed, daemon, virt-launcher/VMI and cleanup work to end. Check
   actual Pods/VMIs and durable reservations, not just terminal run rows. Do not
   reduce allocatable under live work or assume a k3s restart kills containers.
3. **Budget first:** apply the reviewed boot jobs=2/XL=4/runtime jobs=2 change,
   with intake still paused. Verify effective configuration and audit before
   consuming platform capacity. Persist it in `capacity-values.yaml` in the same
   change and verify a render of the next git-tag Helm upgrade retains it. If
   already XL=4, a confirmed #1215 update plus saved-file change is enough.
   Keep the safe boot fallback aligned with the runtime value.
4. **One platform release at a time:** add qualified class/tolerations and
   measured requests, then qualified limits only where their proof exists.
   Roll OpenBao with its operator present: confirm the expected initialized
   state, `sealed=false`/working auto-unseal and authenticated health/token
   delivery before continuing. Pod Running or a permissive sealed health status
   is insufficient. Roll Argo and verify reconciliation; roll Oberth and verify
   DB/run state/token service; roll cloudflared last using independent access
   and verify the external path. Stop immediately if any leg fails. Do not
   coalesce all four restarts into one unobservable change.
5. **Host configuration:** with CI still drained, install the qualified kubelet
   drop-in and restart k3s in the maintenance window. Verify Ready, merged
   thresholds/reservations, post-change allocatable, platform health/unsealed
   OpenBao, class/tolerations/QoS and running container counts. Recalculate the
   CI pool before allowing the canary. Keep disk/IO holds active.
6. **Canary/observation:** one representative run, then at most two M jobs after
   their overlap fits. Observe 30 minutes minimum including a heavy DAG and VM
   cleanup cycle; then 24 hours including a heavy build before considering 3.
   Track Pending p95/max and workflow deadline consumption, CI evictions/day,
   platform restarts/OOM/sealed state, kubelet taint transitions, memory/cache/PSI,
   free bytes/inodes, DB/token/Argo/ingress latency and CPU throttling. Choose
   throughput knowingly; preserve FIFO/aging/head protection and IO ceiling.

Stop intake immediately on **any** platform OOM/eviction/sealed OpenBao, loss of
KubeVirt/CNI/DNS control health, failed
platform recreation, failed DB/token/Argo/ingress health, or a capacity-Pending
canary >60 seconds. Also stop on unexpected CI eviction, repeated pressure taint
flapping (>2 transitions/hour), or Pending p95 >30 seconds over a representative
30-minute window. These are proposed rollout acceptance thresholds to pin before
testing, not measurements or permanent product defaults. The 24-hour acceptance
target is **zero unexpected CI evictions/day**, compared with the retained
baseline. Investigate cache-driven pressure rather than normalizing lost steps.

Give a failed rollout leg at most 10 minutes to recover under paused intake;
otherwise start rollback with the independent host path. Healthy canary
observation is 30 minutes, not an unlimited experiment while runs burn deadlines.
Do not raise CI capacity to compensate for a failed protection rollout.

## 6. Pressure re-creation qualification (not just steady-state survival)

Use a disposable representative k3s/Kubernetes node first, never disk-fill or
OOM the shared host. Keep control-plane access outside the services under test.
Capture exact image/node/kubelet configuration and per-case Pod UID, admitted
numeric priority, resources/QoS, taints, events, readiness and probe results.

The [local 1.36.4 rehearsal](platform-pressure-qualification.md) passed the
namespace, memory/disk taint, real-kubelet disk admission and four changed-UID
stand-in recreation cases using synthetic soft thresholds. It does not qualify
the actual applications, target policy, measured thresholds or resource envelopes.

| Case | Required evidence |
| --- | --- |
| Namespace policy | Server-dry-run each actual replacement Pod in its real namespace; critical class/quota policy admits it. CI attempts to select that class are refused. |
| Memory taint only | Re-create each platform Pod (new UID) while NoSchedule memory taint is present; scheduler binds it, not merely an old Pod survives. CI receives no new blanket toleration. |
| Disk taint only | Same with disk NoSchedule taint; ordinary class without toleration is the scheduler negative control. |
| Real kubelet DiskPressure | Trigger a qualified synthetic threshold on an isolated node without filling host storage; a tolerant custom-priority control reaches kubelet but is rejected, while the system-cluster-critical replacement is admitted and Ready. A manually added taint alone cannot prove this. |
| Real memory pressure/cache | Re-create under measured pressure, verify token/DB/Argo health and unsealed OpenBao; compare active-file-heavy IO with anonymous pressure, grace, hard floor and taint clearing. Record CI evictions and deadline loss. |
| Restart at reduced pool | Re-create under full qualified two-job load; verify priority/headroom recovery and no unexpected peer-platform eviction. Keep KubeVirt/CNI/DNS healthy. |
| Critical storage-limit behaviour | In a bounded disposable fixture, exceed a small disk-backed emptyDir/ephemeral limit with an ordinary control and a critical Pod; distinguish eviction from an actual filesystem quota. Confirm the critical exemption and prove independent log/tmp bounds and reclamation protect CI headroom. Never fill the host disk. |
| KubeVirt priority/preemption | Verify installed numeric priorities/policies, then rehearse demand with 1,000,000,000 KubeVirt control stand-ins and 2,000,000,000 platform replacements: capture victims when CI alone cannot free enough. Confirm reserved KubeVirt recovery headroom/application health; a PDB or class name is not a guarantee against preemption. |
| Recovery | Clear pressure, verify taint transition and CI resumes within the pinned acceptance window, preserving logs/state and bounded retry semantics. |

For every Pod restart, verify **application** health; for OpenBao explicitly
verify initialized/unsealed and a permitted authentication/token operation, and
for ingress verify the remote path. Do not put recovery keys in evidence.
Tolerations + a fake node condition test cover the scheduler only, not kubelet
admission. Local/source tests and target qualification must be reported separately.
If critical-class policy is denied or the real-pressure case fails, platform
protection is **not qualified**: retain paused/reduced CI and use explicit disk
reclamation through the operator path; do not claim tolerations solved it.

## 7. Rollback: capacity down first, capacity up last

On any stop condition, pause intake and lower effective #1215 capacity before
reverting resources/reserves; confirm the change or keep intake paused. Drain
all CI/VM/helper work again. Restore the **exact saved** kubelet configuration:
previous custom drop-in content/mode/owner, previous absence if absent, other
drop-ins and CLI flags. Do not just delete every custom file. Restart k3s as
needed, verify configz/allocatable/Ready and record remaining pressure/taints.

Restore saved platform releases one at a time, each with an application health
check. If the changed host state blocks recovery, restore qualified kubelet
state first through independent access. If OpenBao is down/sealed, restore its
release and unseal/auto-unseal first; verify authentication before depending on
Oberth token delivery. Restore Argo and Oberth/DB health, then ingress last.
Keep the qualified critical class/tolerations while resource rollback needs them;
do not remove their reference/class policy until replacement pods can recover.

Retain the reduced safe boot/runtime budget throughout. Observe restored
platforms and one canary for 30 minutes; only after baseline allocatable,
resource requests, unseal/token/DB health and Pending/eviction rates are proven
may a reviewed CI budget be restored **last**, keeping `capacity-values.yaml`
and the live runtime value aligned for the next git-tag Helm upgrade. Preserve
the coordinator's positive XL=4 decision; rollback is not permission to restore
exclusive XL=0 or an obsolete XL override. Do not restore a saved 4-job budget
onto a still-reduced 7.25-CPU pool. Repeat the full 24-hour watch before increasing
utilisation further. Record the exact restored kubelet and release state.

Schema 36 is forward-only even with sampling off. Helm/resource rollback alone
does not downgrade the DB; use a matching verified pre-upgrade backup and the
appropriate old CLI/image restore procedure. For a v0.17.73 predecessor this is
normally 36→35; skipping back to a schema-34 release needs its matching schema-34
backup. Do not re-use schema35 rehearsal assertions or modify migration ledgers.
See [schema36 release/rollback note](schema36-upgrade-note.md).

## 8. Sampler rollout and stage 2 constraint

Use **15s** on tuxbox. The coordinator/Opus R2 review reports a **PASS** for two
read-only API-proxy probes: five main samples per CPU/current-memory/high-water
family, all with `id` and timestamps, newest age 2.4s; all five main high-water
values nonzero and 60 main filesystem-counter lines. This is supplied evidence,
not a worker's target access; see [probe qualification](kubelet-cadvisor-probe.md).
The server still independently checks full UID/container bindings and
TLS/RBAC. A host-admin API proxy probe does not prove the server's direct-kubelet
TLS/token route: qualify that separately. Missing/malformed metrics stay unknown;
never remove binding/freshness checks just to produce numbers.
The [one-line read-only probe](kubelet-cadvisor-probe.md) prints counts/ages only.

Use `container_memory_max_usage_bytes` as an additional main-container high-water
source where cAdvisor exposes it, falling back to current-usage samples. Keep
lower-bound semantics: counters may be absent/reset and an unobserved/GC'd kill
remains unknown. Copy immutable Workflow observations at sample time, not at each
500ms supervision poll; add chart labels to the separate sampler objects.

**Stage 2 must not lower requests or limits from main-scope figures alone.**
Measure sidecars/helpers/ContainerSets, pod overlap, storage/PVCs and VM guests /
launcher overhead independently; retain conservative authored resources for
missing coverage. A step can move work outside main and sampled peaks can be
censored. No automatic resource tuning or removal of admission is in stage 1.
The sampler stays opt-in; a future IO-pressure write backoff must preserve last
samples/final killed-step evidence rather than discarding it silently.
