# Opt-in platform protection (#1216)

Set `platformProtection.enabled: true` on the Oberth release to create four
chart-managed PriorityClasses and assign the Oberth and cloudflared pod
templates. Default, explicit `false`, and historical stored values without this
key leave rendered workloads unchanged. The chart does not change CI capacity.
For the supplied live installation, retain jobs **4**, weight budget **8**, and
XL **8**, including any runtime capacity ConfigMap. No kubelet settings,
reservations, taints, tolerations, admission rules or sampler settings change.

All four classes have value **1,000,000**, `globalDefault: false`, and
`preemptionPolicy: PreemptLowerPriority`. CI workflows, resource/helper pods and
VM launchers receive none of them; their ordinary priority is zero. Authored
workflow/template priority overrides, pod patches and VMI priority fields remain
refused by existing admission. Class names include both the Oberth namespace
and release identity because PriorityClasses are cluster scoped. For release
`oberth` in namespace `oberth`, they are:

| Component | PriorityClass |
| --- | --- |
| Oberth | `oberth-oberth-platform-oberth` |
| cloudflared | `oberth-oberth-platform-cloudflared` |
| Argo workflow-controller | `oberth-oberth-platform-argo-controller` |
| OpenBao | `oberth-oberth-platform-openbao` |

The Argo controller and OpenBao are **separate Helm releases**, not workloads
owned by this chart. The Oberth installer does not automatically opt them in.
Apply their reviewed overlays separately:

- [Argo chart 1.0.24 overlay](../deploy/platform-protection/argo-workflows-values.yaml):
  `controller.priorityClassName` and `controller.resources.requests`.
- [OpenBao chart 0.28.6 overlay](../deploy/platform-protection/openbao-values.yaml):
  `server.priorityClassName` and `server.resources.requests`.

Adjust the overlay class names to match the rendered Oberth classes if the
release name or namespace differs. Do not configure Argo `workflowDefaults`,
workflow pod priority, executor priority or a cluster global default. Principals
outside Oberth that can create Pods are outside its authored-workflow gate;
cluster policy must restrict their use of platform classes if needed.

## Initial requests and evidence

The owner supplied one `kubectl top` sample at **2026-11-01 01:19 CET**, while
the queue was **idle** (`/tmp/fix/in/platform-usage.txt`). No busy-load samples
exist. These are initial requests with an explicit margin above that sample,
**unmeasured under load**, not measured peaks or safe resource limits.

| Pod | Idle CPU / memory | Initial requests | Margin CPU / memory |
| --- | --- | --- | --- |
| workflow-controller | 12m / 89Mi | 25m / 128Mi | 2.08× / 1.44× |
| OpenBao | 42m / 46Mi | 100m / 64Mi | 2.38× / 1.39× |
| cloudflared | 6m / 26Mi | Existing 50m / 64Mi | Unchanged |
| Oberth | 52m / 42Mi | Existing 200m / 256Mi | Unchanged |

Argo and OpenBao currently have no requests or limits (BestEffort). The overlays
add **125m CPU and 192Mi memory** in total for their single replicas and move
them to Burstable. They add **no CPU or memory limits**. Controller demand grows
with active workflows; an idle sample cannot justify a memory ceiling. Oberth,
its prepare-caches init container, and both cloudflared containers already have
CPU/memory requests; this chart owns no other platform Pod templates. Their
requests, limits and `/tmp` caps stay unchanged. Existing ephemeral requests also
stay unchanged; there is no platform ephemeral-usage evidence for new ones.

## Operator rollout

This is a requests/ordinary-priority first slice. It improves scheduler priority
and kubelet eviction ordering, and removes BestEffort QoS for the two external
releases. It does **not** guarantee survival under kernel OOM or kubelet disk
pressure. These custom classes are not system-critical and do not override
kubelet pressure admission. The broader [protection plan](platform-protection-plan.md)
contains later work and superseded capacity proposals; its jobs=2/budget=4/XL=0
examples are **not** part of this slice or approved live values.

1. Retain existing release pins and values, including the live jobs=4/budget=8/
   XL=8 settings. Render with those values and the opt-in enabled. Review the
   class names and each actual replacement Pod against namespace policy before
   rollout. PriorityClass creation requires cluster-scoped Helm permissions.
2. Apply the reviewed Oberth values change and verify its deployment and token/
   database/ingress health. `watchTunnel.enabled` continues to control whether
   the chart owns a tunnel deployment. If cloudflared is managed elsewhere,
   configure that release's Pod template with the rendered cloudflared class.
3. Upgrade the existing Argo release with its exact current chart pin, retained
   values, and the Argo overlay. Verify the new controller Pod's priority,
   requests, readiness and workflow reconciliation. Keep existing identity,
   controller profile and executor configuration.
4. Upgrade the existing OpenBao release with its exact current chart pin,
   retained values, and the OpenBao overlay, with its operator available to
   verify unseal/auto-unseal and authenticated health. Pod Running alone is
   insufficient. Keep independent operator access for tunnel recovery.
5. Observe representative busy CI, startup/recovery and controller load before
   qualifying larger requests or any limits. Verify admission still reports
   jobs=4, capacity=8, XL=8; monitor Pending time and platform restarts/OOMs.

Higher-priority platform recovery may preempt lower-priority CI pods. This can
fail a started CI step; there is no new retry or replay behavior. Record actual
events rather than claiming an uninterrupted pressure recovery from render
tests. No target cluster or pressure qualification was performed for this slice.

To disable, first remove the priority assignments from Argo/OpenBao (and any
externally managed connector), verify replacement Pods, then turn off the
Oberth opt-in. That removes its assignments and chart-owned PriorityClasses.
Keep the new requests if desired; reverting them returns Argo/OpenBao to
BestEffort. Existing Pods retain their admitted numeric priority until replaced.
