# Where limits live

| Control | Owner | Configuration |
| --- | --- | --- |
| Workflows running at once | Argo | `oberth install --argo-parallelism N`, or Helm `controller.parallelism` / `controller.namespaceParallelism` |
| Container requests/limits and Pod placement | Repository and Kubernetes | Native Argo YAML and cluster admission/quotas |
| Shared step concurrency | Argo and operator | Native mutexes or an operator-created semaphore ConfigMap |
| Workflow/template deadlines and retries | Repository and Argo | Native `activeDeadlineSeconds` and `retryStrategy` |

Oberth has no CPU pool, fair-share queue, size/resource grant, PSI gate, disk
floor, admin hold or startup-stall policy. The [native model](native-scheduling.md)
describes source retention, security boundaries, metadata selectors and the
native retry policy.

## How many Workflows run at once

Argo owns `controller.parallelism` and `controller.namespaceParallelism`.
New managed releases use fixed defaults of **4** globally and **0** per
namespace; `0` means unlimited. These defaults do not depend on node CPU or
capacity. Existing releases keep both values unless the operator chooses a
replacement. The installer reads stored Argo values for Review and reuses
them during upgrades. Existing profile rendering preserves both limits.

Choose values in setup's cluster review, with `ArgoParallelism` and
`ArgoNamespaceParallelism` in the answers file, or with explicit flags:

```sh
oberth install --context <context> --argo-parallelism 8 --argo-namespace-parallelism 3
```

Review distinguishes fresh defaults, requested values and preserved settings.
An omitted input remains omitted during upgrade; reading a value for Review
does not turn it into an override. Pause by scaling the workflow-controller to
zero replicas or using `argo suspend WORKFLOW`.

The managed installer uses Argo Helm values directly. Changing the cap does
not require a content-addressed ConfigMap or a second Oberth release. Existing
operator-set Argo values, including namespace parallelism, are preserved.

## Shared step limits

Operators create and own semaphore ConfigMaps with `kubectl`; see
[native synchronization](native-synchronization.md). The v0.22.0 upgrade stops
rendering `oberth-sync-limits` and orphans the existing object with data unchanged; new installs create required keys themselves.
The managed installer preserves unrelated `extraObjects`, including operator-owned controller configuration. Oberth's own pipelines use named mutexes, not semaphore keys.
