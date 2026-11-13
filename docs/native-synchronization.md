# Native synchronization

Argo owns the Workflow queue, mutexes and semaphores. Kubernetes owns Pod
placement. Oberth submits every validated run without resource admission;
see [native scheduling](native-scheduling.md).

Repository templates or the Workflow itself can declare native locks:

```yaml
spec:
  synchronization:
    mutexes:
      - name: repository-release-finalize
  templates:
    - name: kernel-test
      synchronization:
        semaphores:
          - configMapKeyRef:
              name: oberth-sync-limits
              key: kvm
      container:
        image: YOUR_PERMITTED_PINNED_IMAGE
        command: [./test-kernel]
```

Operators own and create the semaphore ConfigMap themselves. Oberth does not
render it or accept `--argo-sync-limit`. The existing object is orphaned on upgrade with data unchanged; follow the
[release-note handoff](releases/v0.22.0.md#action-required-semaphore-configmap-ownership)
before semaphore Workflows run. Missing ConfigMaps fail in Argo. Authored mutex
names and semaphore references are not rewritten or reserved for an injected
Oberth release lock.

Read-only run observations expose the native waiting reason using the existing
queued status. A lock wait, Argo parallelism postponement or Kubernetes Pending
Pod has no Oberth stall expiry. Authored deadlines, cancellation and native
policy determine when waiting ends. Run cancellation, supersession and
publication remain CI lifecycle operations.
