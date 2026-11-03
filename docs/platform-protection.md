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

The initial requests derive from a **coordinator-supplied idle sample** and a
coordinator instruction to choose a margin. They are **unmeasured under load**,
not owner-approved resource envelopes or measured peaks. The deployment handoff
records the sample, margins and schedulable-capacity cost. Qualify busy load and
startup/recovery before changing sizing or introducing limits. The overlays add
no CPU or memory limits; controller memory demand grows with workflow count.
Existing Oberth/cloudflared requests, limits and temporary-volume caps remain
unchanged.

After replacement Pods actually start with the requests, Argo and OpenBao move
from BestEffort to Burstable. These small memory requests **do not change kernel
OOM victim order in practice**: platform containers remain more exposed than
large-memory CI containers. The priority benefit covers scheduling, preemption
and kubelet eviction ranking only. These ordinary custom classes are not
system-critical; pressure taints and kubelet pressure admission can still block
recreation. No pressure-survival qualification is claimed.

## Activation in a quiet window

Use your deployment's ingress and scheduling controls to pause intake, then drain
all runs, including tag, release, promotion, trusted KVM, source/helper and VM cells.
Keep intake paused through all replacements and health checks; `--wait-idle` is advisory, not an admission freeze. Enabling the
Oberth option restarts Oberth (`Recreate`) and chart-managed cloudflared
(`RollingUpdate`, surge one) in the same Helm upgrade. While Oberth is down, the
executor token barrier stalls and already-started Pod deadlines keep ticking.
Keep independent operator access for tunnel recovery.

Run the following from the checkout containing the reviewed overlays. Set the
release/namespace variables to the actual existing releases. The chart pins
below must match the installed versions; stop and review if they differ. Use the
CLI of the target Oberth release for activation; set `protection_chart` to that
same release's chart for the later same-version disable command. Review
renders against existing namespace policy before making changes; creating
PriorityClasses requires cluster-scoped Helm permissions.
Run these blocks in one subshell so `set -eu` failures do not close the interactive shell; keep the named snapshot directory on persistent storage.

```sh
set -eu
oberth_release=oberth
oberth_ns=oberth
argo_release=argo-workflows
argo_ns=oberth-argo
bao_release=openbao
bao_ns=openbao
bao_pod=openbao-0
argo_chart_version=1.0.24
bao_chart_version=0.28.6
protection_chart=/path/to/reviewed/oberth-chart.tgz
argo_overlay=deploy/platform-protection/argo-workflows-values.yaml
bao_overlay=deploy/platform-protection/openbao-values.yaml
argo_class=$(awk '$1 == "priorityClassName:" {print $2}' "$argo_overlay")
bao_class=$(awk '$1 == "priorityClassName:" {print $2}' "$bao_overlay")
test -n "$argo_class"
test -n "$bao_class"

umask 077
protection_values_dir=$(mktemp -d "$PWD/platform-protection-before.XXXXXXXX")
helm list -n "$oberth_ns" --filter "^${oberth_release}$"
helm list -n "$argo_ns" --filter "^${argo_release}$"
helm list -n "$bao_ns" --filter "^${bao_release}$"
helm get values "$oberth_release" -n "$oberth_ns" -o yaml > "$protection_values_dir/oberth-before.yaml"
helm get values "$argo_release" -n "$argo_ns" -o yaml > "$protection_values_dir/argo-before.yaml"
helm get values "$bao_release" -n "$bao_ns" -o yaml > "$protection_values_dir/bao-before.yaml"
```

Retain these private snapshots and the exact chart/image pins for rollback.
Review a Helm diff or dry-run before each upgrade. **The order below is
required:** establish and verify the classes before changing either external
release. Argo uses `Recreate`; a missing class can remove the old controller and
leave its replacement refused immediately. OpenBao's `OnDelete` hides a wrong
class until the next Pod recreation, when it also prevents replacement.
For external charts, prefer `helm pull` plus a trusted upstream SHA256 check before upgrading from the downloaded archive.

First enable Oberth through the target-release CLI, which merges new chart defaults,
pins the matching server image and compatibility values, and runs schema preflight;
raw Helm `--reuse-values` can retain the old server image under a newer chart.
This also supports same-version re-apply. Then verify its token/DB
health and external ingress. `watchTunnel.enabled` still controls whether the
chart manages cloudflared. An externally managed connector requires its own
Pod-template assignment to the rendered cloudflared class.

```sh
oberth upgrade --namespace "$oberth_ns" --set platformProtection.enabled=true
```

Operators using raw Helm with `--reset-values` must persist
`platformProtection: {enabled: true}` in a dedicated values file passed to every
future upgrade, or the next upgrade deletes the Oberth and cloudflared
PriorityClasses.

Immediately before the Argo upgrade, the exact overlay class must exist with
value 1,000,000 and global default false (which the API may omit). **Stop if any check fails.**
The upgrade must retain existing namespace scope, identities, profile images,
immutable profile ConfigMap and executor settings. Omitting `--reuse-values`
would reset them to chart defaults.

```sh
kubectl get priorityclass "$argo_class"
test "$(kubectl get priorityclass "$argo_class" -o jsonpath='{.value}')" = 1000000
argo_global_default=$(kubectl get priorityclass "$argo_class" -o jsonpath='{.globalDefault}')
test "$argo_global_default" != true
helm upgrade "$argo_release" argo-workflows \
  --repo https://argoproj.github.io/argo-helm --version "$argo_chart_version" \
  -n "$argo_ns" --reuse-values -f "$argo_overlay" --wait --timeout 10m
kubectl get pods -n "$argo_ns" \
  -l app.kubernetes.io/name=argo-workflows-workflow-controller \
  -o 'custom-columns=NAME:.metadata.name,CLASS:.spec.priorityClassName,PRIORITY:.spec.priority,QOS:.status.qosClass'
```

Verify readiness, requests and actual workflow reconciliation before continuing.

Immediately before changing OpenBao, repeat the class check. Preserve its
existing TLS, storage and standalone/HA settings through `--reuse-values`.
**Keep `OnDelete`.** The Helm upgrade changes only the StatefulSet template;
it does not restart `openbao-0` or protect the old Pod. Do not treat Helm success
or the old Pod's health as activation success.

```sh
kubectl get priorityclass "$bao_class"
test "$(kubectl get priorityclass "$bao_class" -o jsonpath='{.value}')" = 1000000
bao_global_default=$(kubectl get priorityclass "$bao_class" -o jsonpath='{.globalDefault}')
test "$bao_global_default" != true
helm upgrade "$bao_release" openbao \
  --repo https://openbao.github.io/openbao-helm --version "$bao_chart_version" \
  -n "$bao_ns" --reuse-values -f "$bao_overlay" --timeout 10m
test "$(kubectl get statefulset "$bao_release" -n "$bao_ns" -o jsonpath='{.spec.updateStrategy.type}')" = OnDelete
```

With the unseal operator present and no credentialed/release runs, explicitly
replace the Pod. **Deleting/restarting OpenBao seals it** until auto-unseal or
manual unseal completes. While sealed, credentialed steps cannot log in. Hosts
without auto-unseal need the operator to unseal by hand. For installations with
more than one server Pod, use the existing HA maintenance procedure one Pod at
a time; the following commands describe the single-server case.
On hosts using systemd auto-unseal, run `systemctl is-active oberth-openbao-unseal` before deletion and inspect `journalctl -u oberth-openbao-unseal --since '-10 minutes' --no-pager` afterward.

```sh
old_bao_uid=$(kubectl get pod "$bao_pod" -n "$bao_ns" -o jsonpath='{.metadata.uid}')
kubectl delete pod "$bao_pod" -n "$bao_ns" --wait=true
kubectl wait --for=create "pod/$bao_pod" -n "$bao_ns" --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Running "pod/$bao_pod" -n "$bao_ns" --timeout=5m
test "$(kubectl get pod "$bao_pod" -n "$bao_ns" -o jsonpath='{.metadata.uid}')" != "$old_bao_uid"
```

Confirm configured auto-unseal completes. Otherwise run this interactive command
for each required key holder until the unseal threshold is met; enter keys only
at the hidden prompt, never in arguments, reports or shell history:

```sh
kubectl exec -it -n "$bao_ns" "$bao_pod" -- bao operator unseal
```

Then confirm `initialized: true`, `sealed: false`, Pod readiness, the exact class,
numeric priority, Burstable QoS and CPU/memory requests on the **new** Pod:

```sh
kubectl wait --for=condition=Ready "pod/$bao_pod" -n "$bao_ns" --timeout=5m
kubectl exec -n "$bao_ns" "$bao_pod" -- bao status -format=json
test "$(kubectl get pod "$bao_pod" -n "$bao_ns" -o jsonpath='{.spec.priorityClassName}')" = "$bao_class"
test "$(kubectl get pod "$bao_pod" -n "$bao_ns" -o jsonpath='{.spec.priority} {.status.qosClass}')" = '1000000 Burstable'
kubectl get pod "$bao_pod" -n "$bao_ns" -o jsonpath='{.spec.containers[?(@.name=="openbao")].resources.requests}{"\n"}'
```

Finally verify authenticated access from the existing operator session (for
example `bao token lookup -field=policies`), plus actual Oberth credential
login/delivery and service health. Do not copy an operator token into the Pod.
Pod Running, readiness or a permissive sealed health response alone is not
acceptance. Stop and recover under paused intake if any leg fails. Resume intake
only after all health checks pass; observe busy CI, Pending time and platform
restarts/OOMs while keeping existing admission capacity.

## Preemption during activation and later restarts

Any lower-priority Pod with useful requests can be a victim: branch, tag,
release/publish, apply, promotion test steps, trusted KVM and virt-launchers.
Platform classes have equal priority and cannot preempt each other. Victim
reprieve favors earlier-started Pods at equal priority, so younger publish/apply
steps can be lost before an older build. Killing a virt-launcher loses in-guest
state. A promotion's upstream push runs inside Oberth, outside the step-Pod
preemption path; its test Pods remain exposed.

Preempted steps currently appear as ordinary step failures and can open CI-red
issues; restart adoption can instead report "server restart interrupted".
Scheduler preemption is not yet classified as infrastructure. Release-like runs
are not automatically replayed, so a publish/apply can leave partial effects.
The same risks apply to **later routine platform restarts**, including the
cloudflared surge Pod needing additional resources. Activation's quiet window
does not protect future restarts under load. Infrastructure classification and
possible server-owned release PDBs are follow-up work; PDB handling during
preemption is best effort.

## Disable, rollback and uninstall

The Argo/OpenBao classes are owned by the Oberth release but marked
`helm.sh/resource-policy: keep` because other releases depend on them. They
remain inert with `globalDefault: false` after disable, rollback or uninstall;
cleanup is manual. Do not remove these annotations to force an out-of-order
cleanup. Oberth/cloudflared classes have the ordinary release lifecycle.

Before disabling, rolling back or uninstalling Oberth, use another quiet window
to restore the external releases' **assignments and requests together**. Keeping
these new requests while clearing the class can leave Argo/OpenBao Pending on a
full node, unable to preempt CI. Restore the saved same-chart values below;
`--reset-values -f` intentionally restores that snapshot rather than merging the
new settings back in. If the snapshot predates other desired changes, prepare
and review an updated restore file first.

```sh
helm upgrade "$argo_release" argo-workflows \
  --repo https://argoproj.github.io/argo-helm --version "$argo_chart_version" \
  -n "$argo_ns" --reset-values -f "$protection_values_dir/argo-before.yaml" --wait --timeout 10m
helm upgrade "$bao_release" openbao \
  --repo https://openbao.github.io/openbao-helm --version "$bao_chart_version" \
  -n "$bao_ns" --reset-values -f "$protection_values_dir/bao-before.yaml" --timeout 10m
```

OpenBao still uses `OnDelete`: repeat the explicit delete, unseal and new-UID/
health verification above, checking its restored class/requests instead of the
protected values. Verify replacement Argo and any external connector Pods too.
Only then disable Oberth with the current feature-capable chart:

```sh
helm upgrade "$oberth_release" "$protection_chart" -n "$oberth_ns" \
  --reuse-values --set platformProtection.enabled=false --wait --timeout 10m
```

A downgrade to a chart predating this feature rejects the stored
`platformProtection` key under `--reuse-values`. After restoring external Pods,
prepare older-chart values with that key removed and use the reviewed older
pins and restore procedure; do not blindly reuse the activated values.
Rollback/uninstall can retain the kept classes even when external consumers
were not restored: running Pods survive, but verify class availability before
any subsequent recreation.

As the **last** cleanup step, after verifying no Pod or workload template still
references either kept class (including external consumers), delete them:

```sh
kubectl delete priorityclass "$argo_class" "$bao_class"
```

Existing Pods retain their admitted numeric priority until replaced. Rendering
and unit tests do not qualify real Helm rollback/uninstall, sealing/unsealing,
preemption or pressure recovery; those remain operator rollout checks.
