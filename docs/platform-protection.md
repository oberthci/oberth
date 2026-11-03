# Platform protection

Set `platformProtection.enabled: true` to create three PriorityClasses for
Oberth, the Argo workflow-controller, and OpenBao. Default, false, and historical
stored values without this key leave workloads unchanged. This does not change
run admission capacity.

All three classes have value 1,000,000, `globalDefault: false`, and
`preemptionPolicy: PreemptLowerPriority`. The chart assigns Oberth's Pod template.
CI workflows do not receive these classes. Admission refuses authored priority
overrides and Pod patches.

For release `oberth` in namespace `oberth`:

| Component | PriorityClass |
| --- | --- |
| Oberth | `oberth-oberth-platform-oberth` |
| Argo workflow-controller | `oberth-oberth-platform-argo-controller` |
| OpenBao | `oberth-oberth-platform-openbao` |

Class names include namespace and release identity. Argo and OpenBao belong to
separate Helm releases: operator-reviewed overlays in
`deploy/platform-protection/` select their classes and add initial CPU/memory
requests. The installer does not apply those overlays. Adjust class names and
qualify requests against your workload before applying them. Priority improves
scheduling, preemption and kubelet eviction ranking; it does not guarantee
survival under memory pressure or change kernel OOM ranking.

Activation requires cluster-scoped permissions. Pause intake, drain work, retain
private copies of current Helm values and chart/image pins, and render all three
releases before changing them. Create and verify classes before assigning the
external releases. The Argo controller uses Recreate. OpenBao uses OnDelete;
changing its template does not update an existing Pod. Follow your deployment's
unseal procedure for replacements and verify new Pod identities, readiness,
requests, classes and credential delivery before resuming intake.

The Argo/OpenBao classes carry `helm.sh/resource-policy: keep` because separate
releases depend on them. Before disabling, rolling back or uninstalling Oberth,
restore those releases' previous assignments and requests together, verify
replacement health, then remove unused retained classes. The Oberth class follows
the ordinary release lifecycle. Never remove a class still referenced by a Pod
or workload template. Historical charts may reject the platformProtection key;
prepare compatible values explicitly for a downgrade.

Lower-priority CI Pods can be preempted during activation or later platform
restarts. Already-published effects are not rolled back. Maintain independent
operator access, and qualify busy-load and recovery behavior before deployment.
