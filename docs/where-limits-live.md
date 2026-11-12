# Where limits live

| Control | Owner | Configuration |
| --- | --- | --- |
| Workflows running at once | Argo | `oberth install --argo-parallelism N`, or controller `parallelism` |
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

Under `nonroot-static-v1`, the cap is carried in the canonical digest-named
controller profile ConfigMap rather than the chart's normal mutable ConfigMap.
Changing a cap produces a new verified profile and controller rollout. Existing
Workflows continue. Explicit profile pins and identity verification remain
security controls; do not edit a digest-named profile's content in place.

## Shared step limits

Operators create and own semaphore ConfigMaps with `kubectl`; see
[native synchronization](native-synchronization.md). The v0.22.0 upgrade stops
rendering `oberth-sync-limits`; recreate required keys before resuming users.
The managed installer preserves unrelated `extraObjects`, including the nonroot
controller profile. Oberth's own pipelines use named mutexes, not semaphore keys.
