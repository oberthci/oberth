# Where limits live

Since v0.20.0 every limit on what runs at the same time lives in Argo (#1517).
Oberth submits each admitted run's Workflow at once; the Argo workflow
controller decides when it starts and which steps wait for which. Oberth keeps
no run ceiling, no concurrency groups and no semaphore table of its own.

| Limit | Where | Set with |
| --- | --- | --- |
| Pipeline Workflows running at once | Argo controller `parallelism` / `namespaceParallelism` | `oberth install --argo-parallelism N` (or the argo-workflows chart values `controller.parallelism`, `controller.namespaceParallelism`) |
| A shared step limit (`kvm: 1`, `heavy-go: 2`) | ConfigMap `oberth-sync-limits` in the pipeline namespace, rendered by the argo-workflows release | `oberth install --argo-sync-limit kvm=1 --argo-sync-limit heavy-go=2` |
| One at a time across runs | a native Argo mutex in the pipeline document | `synchronization.mutexes` in the repository's YAML |
| Releases one at a time | the mutex `oberth-release`, injected by Oberth for every release run | `releases.serialize: true` in the Oberth chart |

What Oberth still holds back, because it reads the host and not a workflow:
the PSI hard valve (`admission.pressure.*`), the disk floor
(`admission.diskFloorBytes`) and the administrator hold (`admission_hold`).
Priority still orders Oberth's queue; see [Ordering](#ordering).

## How many Workflows run at once

The argo-workflows release the installer manages carries the controller's own
limits. `oberth install` changes them only when asked and keeps the release's
values otherwise (`--reuse-values`):

```sh
oberth install --upgrade --argo-parallelism 4
# or, on a release you manage yourself:
helm upgrade argo-workflows argo/argo-workflows -n oberth-argo --version 1.0.24 \
  --reuse-values --set controller.parallelism=4
```

`0` is no limit. The installer's controller is single-namespace, so
`parallelism` and `namespaceParallelism` both bound every pipeline Workflow;
use `parallelism`. The controller watches its ConfigMap, so a change applies
without a controller or Oberth restart.

**Default: none.** Neither Oberth nor the installer sets a limit. That is the
behaviour Oberth has had since v0.18.0, whose `maxConcurrentJobs` default was
`0` (no ceiling): Kubernetes places each step Pod by its `resources`, and the
PSI valve and disk floor still hold new runs on an overloaded host. A useful
number depends on the host, so a generic default would be wrong for most
installs. On a small single-node host, set one.

A Workflow beyond the limit stays `Pending` with Argo's message
`Workflow processing has been postponed because too many workflows are already running`
(Argo v4.0.8, `workflow/controller/controller.go`; no condition is set, and
`startedAt` stays empty). Oberth:

- keeps the run's status and shows the reason line
  `waiting for an Argo workflow slot (controller parallelism)`; no status,
  label or layout is added;
- exempts the wait from `admission.stallTimeout` and from the pre-start
  watchdog, exactly like a workflow-level lock wait: Argo's own deadline
  starts at `startedAt`, so a postponed run burns none of its
  `activeDeadlineSeconds`;
- reports the run in `sync_status` as a waiter on
  `Parallelism/workflow-controller`, and every admitted Oberth Workflow as a
  holder, with history: `max_concurrent_holders` over a window shows the
  highest number of Oberth Workflows Argo ran at once;
- records `run.lock_wait` with `lock: Parallelism/workflow-controller` and
  `postponed: true` once per wait longer than `agentEvents.lockWaitThreshold`
  ([agent events](agent-events.md)).

### What occupies a slot

Argo counts a Workflow from the moment its throttler admits it until it is
complete. That includes Workflows that are admitted but still waiting:

- a release waiting for `Mutex/oberth-release` (workflow-level mutex), and any
  Workflow waiting for a workflow-level semaphore or mutex of its own;
- a Workflow with a step waiting for a template-level lock;
- an assisted retry hold: the held run's Workflow is running, so it keeps its
  slot for up to the step budget plus `assist.holdTTL`, and at most
  `assist.maxConcurrentHolds` held runs do so at once.

A postponed Workflow holds no lock (Argo takes locks only after admitting it),
so a lock holder is always admitted and the queue cannot deadlock. It can stall
throughput: with `parallelism: 2` and two releases waiting for the first
release, branch runs wait too. Size the limit for the waiters you expect, or
put locks on templates instead of the whole Workflow. After a controller
restart Argo counts only `Running` Workflows until it has re-processed the
`Pending` ones, so the limit can be exceeded briefly.

### Ordering

Oberth's queue order (priority `low < standard < promotion < high`, then
FIFO) decides the order in which runs are submitted. Argo admits postponed
Workflows by `spec.priority`, then creation time; Oberth sets no
`spec.priority` and documents cannot set one, so among postponed Workflows the
earlier submission starts first. A release submitted after ten postponed
branch runs waits behind them. Raise the limit or hold ordinary work
(`admission_hold`) when release order matters.

## Shared step limits

A step that must respect a shared limit declares the native Argo semaphore,
unchanged since v0.18.0:

```yaml
templates:
  - name: kernel-matrix
    synchronization:
      semaphores:
        - configMapKeyRef:
            name: oberth-sync-limits
            key: kvm
```

The ConfigMap keeps its name and namespace, so no repository changes. The
argo-workflows release renders it through its `extraObjects`:

```sh
oberth install --upgrade --argo-sync-limit kvm=1 --argo-sync-limit heavy-go=2
```

The set given replaces the previous one; omitting the flag keeps it. With your
own argo-workflows release, the equivalent values are:

```yaml
extraObjects:
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: oberth-sync-limits
      namespace: oberth-argo
    data:
      kvm: "1"
      heavy-go: "2"
```

The installer owns `extraObjects` of the release it manages; keep other extra
objects in a release of your own. Argo re-reads the ConfigMap, so a change
applies to the next acquisition. Holders, waiters and history are in
`sync_status`, and a long wait is `run.lock_wait`.

### Moving the semaphore limits

v0.18 and v0.19 rendered `oberth-sync-limits` from the Oberth chart's
`argoSync.semaphores`. Helm deletes an object that leaves a release, and a
deleted ConfigMap fails every running Workflow whose semaphore names it. Hand
it over before the Oberth upgrade:

```sh
oberth install --upgrade --argo-sync-limit kvm=1 --argo-sync-limit heavy-go=2
```

finds the existing ConfigMap, marks it for adoption by the argo-workflows
release with `helm.sh/resource-policy: keep`, and renders it from that release.
Without the installer:

```sh
kubectl -n oberth-argo annotate configmap oberth-sync-limits --overwrite \
  helm.sh/resource-policy=keep \
  meta.helm.sh/release-name=argo-workflows meta.helm.sh/release-namespace=oberth-argo
helm upgrade argo-workflows argo/argo-workflows -n oberth-argo --version 1.0.24 \
  --reuse-values -f argo-limits.yaml   # the extraObjects above
```

Then remove `argoSync` from the Oberth values file. The Oberth chart refuses a
non-empty `argoSync.semaphores`, and refuses to upgrade while the live
ConfigMap still belongs to the Oberth release without `keep`, so it is never
deleted by accident.

## Migrating from concurrency groups

`oberth.ci/concurrency-group` and `oberth.ci/concurrency-limit` are accepted
and ignored since v0.20.0, like `oberth.ci/exclusive` since v0.18.0: a
document that still carries them runs, without the limit. A group of one is a
workflow-level native mutex; the name is shared across repositories and
triggers, as the group was:

```yaml
# before
metadata:
  annotations:
    oberth.ci/concurrency-group: terraform-state
spec:
  entrypoint: ci

# after
spec:
  synchronization:
    mutexes:
      - name: terraform-state
  entrypoint: ci
```

A group with `oberth.ci/concurrency-limit: N` is a semaphore: add the key to
`oberth-sync-limits` (`--argo-sync-limit <group>=N`) and declare it under
`spec.synchronization.semaphores` with `configMapKeyRef`. Put it on the
templates that touch the shared state instead of the Workflow when the rest of
the run may proceed; see [native synchronization](native-synchronization.md).

Differences to know:

- Argo orders a mutex queue by `spec.priority`, then creation time, not by
  Oberth's queue order.
- A Workflow waiting for a workflow-level mutex occupies an Argo parallelism
  slot ([above](#what-occupies-a-slot)); a parked group member did not.
- A scoped (non-admin) uplink may cancel a branch run that holds a mutex;
  concurrency-group members could be cancelled only by an admin. Release, tag,
  apply and scheduled runs stay admin-only.
- The `concurrency_groups` MCP tool and `oberth admission groups` are gone;
  `sync_status` reports mutex and semaphore holders, waiters and history.
