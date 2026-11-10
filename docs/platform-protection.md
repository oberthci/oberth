# Platform protection

Set `platformProtection.enabled: true` to create three PriorityClasses for
Oberth, its Argo workflow-controller and OpenBao. Default, false, and historical
stored values without this key leave workloads unchanged. The classes are
cluster-scoped and preempt lower-priority Pods, so on a node shared with other
workloads they are an explicit choice. (Chart versions v0.17.79 through v0.20.x
defaulted to true; since v0.21.0 an `oberth upgrade` of an install that never
set the key removes the classes and the server's `priorityClassName`. Pass
`--set platformProtection.enabled=true` once to keep them.) Run admission capacity
is unchanged. External ingress, connectors and other projects have their own
protection policy outside Oberth.

All three classes have value 1,000,000, `globalDefault: false` and
`preemptionPolicy: PreemptLowerPriority`. The chart assigns Oberth's Pod template.
CI Workflows cannot select these classes: admission refuses authored priority
overrides and Pod patches.

For release `oberth` in namespace `oberth`:

| Component | PriorityClass |
| --- | --- |
| Oberth | `oberth-oberth-platform-oberth` |
| Argo workflow-controller | `oberth-oberth-platform-argo-controller` |
| OpenBao | `oberth-oberth-platform-openbao` |

Names include namespace and release identity. Argo and OpenBao belong to separate
releases. The priority examples in `deploy/platform-protection/` are optional and
are not applied by the installer. Adjust their class names to the rendered
Oberth chart. They do not set resource requests or limits: preserve the target
installation's existing values and qualify sizing separately. No installation's
idle sample is a product resource default.

Priority affects scheduling, preemption and kubelet eviction ranking. It does
not guarantee survival under memory pressure or set kernel OOM victim order.
These ordinary custom classes are not system-critical; pressure taints and
kubelet admission can still prevent replacement Pods from starting. Resource
requests, available capacity, QoS and eviction policy remain relevant.

## Activation and verification

Use a maintenance window and independent operator access. Pause intake using
the installation's controls and drain all work, including credentialed steps
and legacy execution. `--wait-idle` observes idleness; it does not freeze intake.
Keep intake paused until replacement and health checks complete.

Save private copies of each release's values and exact chart/image pins. Render
Oberth and both dependency charts against those values. Activation requires
cluster-scoped permission to create PriorityClasses. Establish and verify the
classes before assigning the separate dependency releases; a missing class can
prevent replacement of their Pods.

Enable Oberth with the target-release CLI so image pins, compatibility values
and schema preflight are retained:

```sh
oberth upgrade --namespace "$OBERTH_NAMESPACE" --set platformProtection.enabled=true
```

For separate dependency releases, merge only the reviewed priority example into
stored values using their existing chart versions. Preserve namespace scope,
identities, TLS, storage, controller profile and resource sizing. Oberth and Argo
use Recreate: their replacement temporarily interrupts service. OpenBao uses
OnDelete: changing its template alone does not replace or protect an existing
Pod. Preserve that strategy and use the installation's authorized replacement
and unseal procedure. A replacement seals OpenBao until configured auto-unseal
or the required key holders complete unseal. Never put unseal keys in arguments.

Verify new Pod UIDs, the intended class and numeric priority, unchanged resource
requests/limits, readiness, workflow reconciliation, database access and actual
credential delivery. Helm success or a Running Pod alone is insufficient. For
HA installations use their existing staged maintenance procedure. Keep rollback
configuration and operator access available until acceptance is complete.

## Preemption and recovery

Any lower-priority CI Pod can be preempted, including a release or promotion
step. A lost VM launcher also loses guest state. Equal-priority platform Pods
cannot preempt one another. A promotion's upstream push runs inside Oberth;
its workflow test Pods still face preemption. A publish/apply step can leave
partial external effects and is not made safe by merely replaying a run.

The same risks apply to later routine restarts. A quiet activation window does
not qualify recovery under load. Use the
[platform recovery qualification](platform-pressure-qualification.md) procedure
and keep installation-specific measurements in external operator records.

## Disable, downgrade and uninstall

The Argo/OpenBao classes carry `helm.sh/resource-policy: keep` because separate
releases depend on them. The Oberth class follows the ordinary release lifecycle.
Before disabling protection, downgrading or uninstalling Oberth, restore the
external releases' previous assignments together with their resource settings.
Verify replacement health before removing any unused retained class. Never
remove a class still referenced by a Pod or workload template. Existing Pods
retain their admitted numeric priority until replaced.

Charts predating this feature may reject stored `platformProtection` values.
Prepare compatible older-chart values explicitly instead of blindly reusing
new settings. Retention annotations are not a substitute for orderly migration.
For resources from a release that previously owned external ingress, use the
[ownership handoff and keep procedure](ownership-handoff.md) before upgrading.
