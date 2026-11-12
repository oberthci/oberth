# Platform protection

The chart always creates two scheduling classes. One shared control-plane
PriorityClass has value 1,000,000 and `preemptionPolicy: PreemptLowerPriority`.
The Oberth server uses it, and the installer passes the same class to the Argo
controller and OpenBao through their chart values. CI saturation therefore does
not leave a replacement server waiting behind pipeline CPU requests.
On an eligible node, this class can preempt any lower-priority Pod, including
workloads outside Oberth.

The pipeline class has value 0 and `preemptionPolicy: Never`. Workflows, source
seed/collection Pods and executor identity helpers use this lower class. Pipeline
work does not preempt other pipelines. Both classes have `globalDefault: false`,
so unrelated Pods do not inherit them. Repository-authored Workflow, template
and Pod-patch priority overrides are refused, including release runs. Resource
manifests and embedded resource Pod templates cannot select a priority class or
supply Pod patches. Argo's
release queue priority only orders Workflows; it never raises Pod priority.

For Oberth release `oberth` in namespace `oberth`, using pipeline namespace
`oberth-argo`:

| Workloads | PriorityClass |
| --- | --- |
| Oberth, Argo controller, OpenBao | `oberth-oberth-control-plane` |
| Pipeline Pods and helpers | `oberth-argo-oberth-pipeline` |

Control-plane names include the Helm release fullname and namespace. The
pipeline class belongs to its execution namespace, which has one execution
owner. An isolated execution target must establish the non-preempting class
`<pipeline-namespace>-oberth-pipeline` in that cluster as well.

The installer creates the classes with the final Oberth Helm ownership before
installing OpenBao or Argo; missing classes would otherwise prevent their Pods
from being admitted. The Oberth chart then adopts those same objects. An existing
class with conflicting ownership or priority policy refuses installation.
Creating them requires cluster-scoped PriorityClass permission in the selected
kubeconfig context. The classes carry `helm.sh/resource-policy: keep` because
separate dependency releases and active Pods can outlive the server release.
Remove them only after those releases and pipeline workloads are gone.

Priority classes are mandatory. If the stored Helm values contain
`platformProtection`, clear that setting with
`oberth upgrade --set platformProtection=null`; the chart refuses that toggle.

For separately managed dependencies, the examples in `deploy/platform-protection/`
set their priority and small CPU/memory requests. Use the names rendered for the chosen Oberth release.
Review requests against actual usage. Apply the default overlay before your
explicit sizing values (`-f deploy/platform-protection/argo-workflows-values.yaml
-f custom-values.yaml`), preserving TLS, storage, identities and controller profiles.
OpenBao uses OnDelete: updating its template protects replacement Pods and does
not restart or unseal the current Pod. Use the normal authorized replacement and
unseal procedure. Never put unseal keys on command arguments.

## Server CPU

The server requests 500m CPU and omits a CPU limit by default. The request
reserves capacity under this PriorityClass; leaving CPU uncapped allows bursts
without a per-container quota. See [measurements and overrides](server-cpu.md).

## Verification

Render the Oberth and dependency charts and inspect each Pod template's
`priorityClassName`, the class values and preemption policies. With disposable
pipeline work filling CPU requests on a suitable node, restart the server or
perform the normal upgrade. Verify a new server Pod reaches Ready while lower
priority pipeline Pods are preempted. The coordinator runs this live acceptance;
fake-client and rendering tests cannot measure a real recovery deadline.

Priority makes CPU requests reclaimable. It does not add capacity or override
node affinity, taints, unavailable storage, image pulls or memory pressure.
Requests, QoS and node health still matter. See [CPU admission](cpu-admission.md)
and [host preparation](host-preparation.md).

## Dependencies and user-added ingress

Managed OpenBao and Argo controller Pods request 100m CPU and 128Mi memory.
For a user-added ingress or tunnel Deployment, set its Pod template
`priorityClassName: oberth-oberth-control-plane` (use your rendered class name)
and give each container positive requests, for example:

```yaml
spec:
  template:
    spec:
      priorityClassName: oberth-oberth-control-plane
      containers:
        - name: tunnel
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
```

Merge this into the existing Deployment, retaining its image, arguments and
other settings. Size requests using observed usage. Only dependencies needed
for the control plane should use this preempting class.
