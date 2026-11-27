# Capacity maintenance and qualification

This is an operator runbook. Configuration candidates are unqualified until the
receipts below exist. Do not raise the run cap from this document's examples.
Retention/shorter TTL and permanent cap-guard retirement need a separate approved
protection-safe retention design. Keep the current TTL and collection barrier.

## S1a: host profile

The complete candidate drop-in is
[`kubelet-profile.yaml`](../../internal/installer/kubelet-profile.yaml).
It keeps maxPods 110, systemReserved 1500m/4Gi and enforcement on Pods only.
Do not add another reserve, reserved-cgroup enforcement, swap capacity or a CPU
governor change. The profile includes both filesystem inode thresholds because
partial hard-eviction maps disable omitted thresholds with default merging off.
The shared image/node filesystem needs no independently configured containerfs
threshold. Requalify the profile if the runtime's filesystem topology changes.

For the supported k3s version (>=1.32), the operator places the reviewed drop-in
after `00-k3s-defaults.conf` in the effective kubelet configuration directory
(normally `/var/lib/rancher/k3s/agent/etc/kubelet.conf.d/`). First save exact prior
drop-ins and effective public kubelet flags; remove conflicting kubelet CLI flags
from their actual owner configuration. Flags override drop-ins. Never edit the
generated containerd configuration or install a privileged host-mutator/purge cron.
References: [k3s configuration](https://docs.k3s.io/installation/configuration#kubelet-configuration-files),
[kubelet API](https://kubernetes.io/docs/reference/config-api/kubelet-config.v1beta1/).

Each disruptive layer needs a separate naturally drained window:

1. Save installed release/chart/images, rendered templates/checksums, values,
   public node config, quotas, namespaces and obligations. Verify a usable backup
   and selected administrator profile's unseal capability without displaying any
   recovery material. Assign observation and recovery ownership. Identify and
   disable the specific external cap-guard process and launch hook before
   maintenance; if this cannot be verified, defer. Do not restore reinstall logic
   on rollback. Permanent retirement remains blocked by retention prerequisites.
2. Coordinate pausing submissions and native schedules. Let existing Workflows,
   retry/hook/VM/Job children, held work, archives and publications finish. Check
   every affected namespace and outstanding obligation, not just Workflow phase.
   A positive lower cap only reduces new starts; zero means unlimited. Keep
   collection running. Do not force-cancel work to make the window.
3. For S1a only, apply the saved/reviewed drop-in and restart k3s once. Re-read
   node configz and status. ReviewHost must match the effective kubeletconfig;
   recompute allocatable from node status (10% nodefs reserve is approximately
   844Gi on the measured filesystem). A file on disk is not effective evidence.
4. Before reopening: node Ready and no pressure, store actually unsealed (its
   native readiness probe also accepts sealed state), server/controller Ready,
   source/grant checks and API probes passing for ten minutes. Inspect one
   ordinary run's identities, requests, events and archived logs before widening.
5. Host rollback restores exact prior drop-ins/flags at drain and restarts once;
   repeat effective-config/readiness checks. It cannot undo eviction or deleted
   runtime content. Never reinstall another release as a pressure response.

G7: sample node/Pod/status/events/API every 20s, directories every 5min and archive
receipts every minute. Stop expansion on any pressure, unexpected restart,
control OOM/eviction/readiness miss, API p95 >1s or >110% of busy baseline over
5min (>=15 samples), nodefs <20%, directory >=90% budget, or threatened drain
reserve. The hard free-space acceptance floor is 15%. Pause new arrivals, keep
collection running, drain safely and diagnose; reverse configuration at its
proper boundary. Archive backlog growing in two one-minute samples or a terminal
archive incomplete after five minutes also stops expansion. Require ten minutes
healthy before reopening and >=48h/two busy bursts for ordinary-work acceptance.
Missing G1 identities, high-water measurements or staffed observation blocks
qualification. Record planned maintenance separately from ordinary-work outages.

## S1b: controls

`install` and `upgrade --resources '<JSON>'` accept native requests/limits keyed
by server, controller, store and executor. The full installer `--detail` review
shows values. Change one component per drained window; preserve all other saved
choices and the installed release. The server value is `resources`; the other
values are `argoNative.controller.resources`, `storeNative.server.resources` and
`argoNative.executor.resources`. Defaults retain CPU/memory requests and the
store's 512Mi limit. No default CPU or new memory limit is imposed.

Derive each candidate's requests/limits from G1 high-water/cold source
measurements, including the largest supported input. No control sizing preset
is supplied. Server tmp can reach 2Gi, and PVC state is separate. Record final
Pod requests/limits and exact old component template. Server/controller use Recreate;
store uses RollingUpdate. After a store restart use
`oberth --profile <saved-profile> unseal`, then repeat the same recorded
install/upgrade completion when required. Pod readiness alone cannot prove
unseal. Restore the exact prior resource values at drain for rollback; unseal
again if store restarts. Do not use release rollback for a resource-only trial.

Off-production G2 recovery fixture protocol (mandatory, not executed by render
tests): freeze the full Pod inventory and pinned versions; fill the qualified
aggregate run CPU/memory/Pod quota using supported children and largest inputs.
Retain native objects/events, 20s API samples, memory high-water/throttle and
pressure/OOM/eviction counters. Replace server, controller and store separately
at that full load and test worst permitted non-run/maintenance overlap, including
higher preempting system classes. Record replacement UIDs, scheduling and Ready
times, store seal/unseal and successful source/grant/log checks. Deliberately
under-request CPU/memory and exceed a run limit: a fault run may OOM; no control
OOM/eviction, node pressure or out-of-budget host/device usage may occur. Require
ten minutes healthy API recovery and an explicit return to previous values.
Unknown shapes, absent receipts or failed containment blocks activation.

## S1c: selected namespace policy

Default installs have no capacity quota or LimitRange. `--run-policy '<JSON>'`
opts selected existing namespaces into native defaults and six unscoped quota
dimensions. `run-policy-candidate.json` assigns the entire proposed envelope to
one namespace. Carve child shares out of that total; never duplicate it. Helm
rejects aggregate allocations above 9000m CPU, 49152Mi memory, 80 Pods or 256Gi
ephemeral, including on upgrades. This leaves the measured non-run reserve of
1.5 CPU/8.834445953Gi/30 slots; baseline plus worst maintenance overlap must fit
inside it. The separate control-priority exclusion is retained in every selected
namespace. The chart creates no child namespace, cross-namespace execution right
or repository binding. Namespace ownership/child authority remain native owners.

Before activation, owners must complete `inventory.csv` for every admitted ref,
entrypoint/template/templateRef/CronWorkflow, hook/retry, sidecar, restartable init,
source artifact plugin, secret-install helper and generated Job/VM launcher/helper.
Include Pending and existing nonterminal Pods, guest/QEMU/device/Pod overhead,
tmpfs and all child destinations. Record native final defaulted Pod specs and
largest cold/warm inputs on the pinned stack, not just authored main resources.
Unknown/dynamic shapes or children outside quota-covered namespaces block G2.
The empty inventory shipped here is a receipt format, not a completed inventory.
Use the existing namespace/RBAC binding to restrict all child destinations; audit
all allowed service accounts. Keep the KVM gate until the separately reviewed
VM-count/scope change is qualified. LimitRange cannot select only plugin containers.

Retain these deciding native admission fixtures in repository CI on the pinned
API/controller (offline rendering does not establish their native outcomes):

| Input | Required observation |
| --- | --- |
| request-only ephemeral 16Gi or memory >512Mi | Native rejection against smaller default limit; migrate authored limits >=request. Outside policy, valid input remains valid. |
| limit-only ephemeral 256Mi / CPU 50m | API defaulting copies missing requests from limits before LimitRange; retain final values for all injected containers. |
| explicit request/limit pairs | Preserved byte-equivalent quantities; no clamping or rewriting. |
| intentionally empty resources | Defaulted in selected namespaces; defer namespace policy if unlimited semantics are required. |
| sidecar/restartable-init/plugin/VM/Job | Materialize every final shape, prove init overlap/overhead and namespace quota accounting. |
| missing required resources / quota overflow | Native Pod-create 403; retain actual Argo step or VM/Job reconcile result and explicit recovery. No automatic rerun or queue. |
| too-large concurrent fan-out / under-request / over-limit | Quota refusal and contained run OOM; G2 control replacements and API recovery still pass. |

Require disk emptyDir.sizeLimit in selected repository CI. Count shared volumes
once, layers/logs separately and tmpfs against memory. PVC growth is outside Pod
ephemeral quotas. Native quotas bound admitted totals, not physical reservation.
Keep limits unqualified until short peaks/cold extraction and full-load faults
pass. Apply executor resources, namespace defaults and quotas in their own drained
layer after migration; give secret-install helpers authored native resources.
The pinned artifact driver has no resource field; final LimitRange accounting is
required. Deleting policy affects future Pods only. Roll back exact prior
executor/default/quota values after a drain; existing defaults, failed Pod creates
and evictions are not undone. Do not loosen quotas automatically under load.

## S4: offline evidence review and native classes

`oberth install capacity --print-host-profile` prints the drop-in only.
`oberth install capacity --output json < evidence.json` reviews a frozen snapshot
without reading a cluster/profile or applying anything. Text also lists host
config differences. Neither ordinary installation nor push calls the sizing
function. Supply an object matching `installer.CapacityEvidence`: snapshot/time,
currentCap/origin, effective host kubeletconfig, native Node, final native
nonRunAndMaintenance Pods (worst permitted overlap), unscoped native run Quotas,
operatingScratch, families and receipt artifact references G1/G2/G3/G7/placement.
Each family records name, frozen mixWeight, executions>=20, sampledPods/joinedPods
(>=99%), unknownShapes=false, active intervals with seconds/final nonterminal
Pods, and worstPods for the largest supported concurrent shape. Preserve Pod UIDs
and namespace identity. No zero-Pod lock-wait dilution is accepted. Include every
helper, hook, retry and VM/Job child. Placement receipts must cover devices, PVCs,
affinity/anti-affinity, RuntimeClass/overhead and actual pinned native replay.
Positive device/hugepage requests are outside this sizing seam's four-dimensional
model and yield advice unavailable; qualify their explicit bound separately.
Pod affinity, topology spreading, dynamic claims and scheduling gates also need
separate native qualification; this seam declines those placement shapes.
Receipt references are operator-reviewed evidence, not authenticated attestations.
Missing retention G3 currently prevents a qualified recommendation.

The proposal uses mix/time-weighted nearest-rank p95 concurrent requests and
B=min(aggregate quotas, allocatable minus non-run/maintenance budgets, scratch
operating envelope). Native helpers account init-sidecar overlap, Pod resources
and overhead. All six worst-case quota dimensions must fit at the proposed cap;
memory/ephemeral limits must be known and positive. No placement or unknown shape
means advice unavailable, never cap zero. The output preserves the chosen cap,
origin, evidence time and calculation vectors (CPU millicores, memory bytes,
ephemeral bytes, Pods, memory-limit bytes, ephemeral-limit bytes). A trial is
still subject to native replay and adjacent-cap throughput/health qualification.

`--run-priorities '{"release":10000,"ordinary":0}'` renders explicit class names
`oberth-run-<installation-namespace>-<suffix>`, all Never, below control priority,
and never global defaults. Repository owners select native Workflow/Pod priorities;
this seam assigns none. Classes carry Helm keep so older-chart rollback retains
class names needed by pending/authored work. Reclaim retained classes manually
only after references drain; this chart does not own repository namespace migration.
Cap-only changes preserve all three control templates/checksums and use native
live reload. Missing evidence never invokes a restart or changes runtime limits.

Resource rollback can explicitly clear a trial field with JSON null, for example
`{"server":{"requests":{"ephemeral-storage":null},"limits":{"ephemeral-storage":null}}}`.
Use saved exact values; omitted fields preserve saved choices. Read the full
rendered diff and restore CPU/memory defaults if they were changed explicitly.

`python3 hack/capacity/fixtures.py --namespace <isolated-test-namespace> --image
<qualified-python-image@sha256:digest> --case admission|saturation|overlimit`
prints native fixtures only. The admission List contains deliberate refusals;
submit individual objects to retain each native response, then capture final
injected/defaulted objects and the pinned Argo outcome separately. Saturation is
a finite 80-Pod Job with no retry and aggregate requests 8960m/49120Mi/240Gi;
account any VM/Job/controller children by carving shares before use. Its actual
memory working set is below the quota; record high-water and add qualified
largest-input/VM/device workloads for full G2. Never run these loads in production.
Observe a separate 700Mi/614Mi over-limit run for contained OOM. These fixtures
need the selected off-production cluster and a qualified Python image; generating
them is not a G2 receipt. Retain native quota-overflow 403 responses as well as
successful recovery after explicit cleanup. No fixture starts a VM or applies
objects. For child namespaces outside the installation's two native namespaces,
the installer deletion guard stays narrow: at a drained rollback boundary the
operator removes the exact owned child policy objects separately before reverting
the chart. Do not widen execution authority or bypass namespace ownership checks.
