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

`0` parallelism is unlimited. Pause by scaling the workflow-controller to zero
replicas or using `argo suspend WORKFLOW`. The fresh-install default is
unchanged: half observed node allocatable CPU, minimum 2, within the installer's
maximum; no limit is installed if capacity cannot be observed. On an existing
install, explicit settings change the value and omission preserves it.

The managed installer uses Argo Helm values directly. Changing the cap does
not require a content-addressed ConfigMap or a second Oberth release. Existing
operator-set Argo values, including namespace parallelism, are preserved.

## Shared step limits

Operators create and own semaphore ConfigMaps with `kubectl`; see
[native synchronization](native-synchronization.md). The v0.22.0 upgrade stops
rendering `oberth-sync-limits` and orphans the existing object with data unchanged; new installs create required keys themselves.
The managed installer preserves unrelated `extraObjects`, including operator-owned controller configuration. Oberth's own pipelines use named mutexes, not semaphore keys.
