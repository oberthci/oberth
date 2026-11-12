# Where limits live

Since v0.21.10 Oberth packs whole-run CPU reservations before submission.
Argo owns workflow parallelism and native locks; Kubernetes places each Pod.
See [CPU admission](cpu-admission.md) for the capacity defaults and overrides.

| Limit | Where | Set with |
| --- | --- | --- |
| Pipeline Workflows running at once | Argo controller `parallelism` / `namespaceParallelism` | `oberth install --argo-parallelism N` (or the argo-workflows chart values `controller.parallelism`, `controller.namespaceParallelism`) |
| A shared step limit (`kvm: 1`, `heavy-go: 2`) | ConfigMap `oberth-sync-limits` in the pipeline namespace, rendered by the argo-workflows release | `oberth install --argo-sync-limit kvm=1 --argo-sync-limit heavy-go=2` |
| One at a time across runs | a native Argo mutex in the pipeline document | `synchronization.mutexes` in the repository's YAML |
| Releases one at a time | the mutex `oberth-release`, injected by Oberth for every release run | `releases.serialize: true` in the Oberth chart |

Oberth also holds new work for bounded host PSI deferral, the disk floor
(`admission.diskFloorBytes`, 1 GiB free by default; 0 disables it), and the
administrator hold (`admission_hold`). Priority and repository fair share order
admission; see [Ordering](#ordering).

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
without a controller or Oberth restart, except under the nonroot profile
(below).

### Under the nonroot controller profile

With `argo.controllerProfile: nonroot-static-v1` the controller does not read
the chart's own ConfigMap (`controller.configMap.create: false`), so the
chart never renders `controller.parallelism`. The limits travel inside the
immutable profile ConfigMap instead: its `config` is the built-in profile plus
`parallelism` / `namespaceParallelism` when positive, rendered canonically
(JSON with sorted keys), and its name is `oberth-argo-nr-` followed by the
first 48 hex digits of the SHA-256 of that `config`. Without limits it is
exactly the built-in `oberth-argo-nr-5f121f7f…`; with `--argo-parallelism 6`
the `config` is

```json
{"failedPodRestart":{"enabled":false,"maxRestarts":3},"nodeEvents":{"enabled":true},"parallelism":6,"workflowEvents":{"enabled":true}}
```

`oberth install --upgrade --argo-parallelism N` on a profile release keeps the
profile, renders the matching ConfigMap and points the controller at it. The
Oberth release pins no name (`argo.controllerProfileConfigMap` stays empty, and
the installer clears an earlier pin): before each nonroot submission the server
reads the profile ConfigMap the running controller names in `--configmap` and
verifies that its data is exactly the canonical profile its digest-derived name
stands for; any other key or byte is refused. The server's Role
`<release>-argo-controller-profile` grants `get` (no list, watch or write) on
ConfigMaps in the pipeline namespace for this. A changed limit is a new
immutable ConfigMap, so the controller rolls once; running Workflows survive
the rollout. Changing the limits needs no Oberth upgrade and no release order,
and nonroot admission does not pause.

Since v0.21.0. Before it, the Oberth chart pinned the name in its Role and the
server flag, so admission paused between the argo-workflows and the Oberth
upgrade. That pause happens one last time while upgrading to v0.21.0 (the old
server still holds the pin); afterwards it does not recur. A name set in
`argo.controllerProfileConfigMap` by hand still pins exactly that profile.
`oberth upgrade` clears a stored pin that names the profile the controller
runs now (the value an earlier installer wrote) and prints that it did; a
pin naming another profile is left as set.

The profile is layered over the argo-workflows release's stored values only
when they are ones it knows: its own keys, the limits, the controller's
`priorityClassName`, `resources.requests`, `nodeSelector` and `tolerations`.
Anything else refuses the profile or limits run, because the installer cannot
tell whether it changes what the controller runs. To keep such values, review
them and acknowledge exactly those bytes:

```sh
helm get values argo-workflows -n oberth-argo -o json        # review
helm get values argo-workflows -n oberth-argo -o json | sha256sum
oberth install --upgrade --argo-parallelism 6 --argo-values-reviewed sha256:<that digest>
```

`--argo-values-reviewed` (also on `oberth upgrade --argo-controller-profile`)
binds the review to those values: any later change refuses again. The
controller's image, arguments, environment and mounts are verified against
the profile after the upgrade either way.

**Default: half the cluster's CPUs.** Since v0.21.0 a fresh `oberth install`
that creates the argo-workflows release sets `parallelism` to half the
cluster's allocatable CPUs, at least 2 (it prints the number). An explicit
`--argo-parallelism N` wins, `0` is no limit, and an existing release keeps
whatever it has: an upgrade never adds a limit. With no Argo limit, Oberth still packs CPU requests before submission.

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
  slot for up to the step budget plus two `assist.holdTTL`, and at most
  `assist.maxConcurrentHolds` held runs do so at once.

A postponed Workflow holds no lock (Argo takes locks only after admitting it),
so a lock holder is always admitted and the queue cannot deadlock. It can stall
throughput: with `parallelism: 2` and two releases waiting for the first
release, branch runs wait too. Size the limit for the waiters you expect, or
put locks on templates instead of the whole Workflow. After a controller
restart Argo counts only `Running` Workflows until it has re-processed the
`Pending` ones, so the limit can be exceeded briefly.

### Ordering

Oberth orders admission by `low < standard < promotion < high`, then fair
share across repositories and FIFO within each repository. Argo receives
`spec.priority` -1, 0, 1 and 2 respectively; release runs have at least 1.
Argo orders postponed Workflows and lock waiters by this value, then creation
time. Repository documents cannot set priority. This queue order does not
raise Pod priority or preempt running work. See [CPU admission](cpu-admission.md).

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

Then drop `argoSync` from the Oberth release: the value no longer exists, and
`oberth upgrade` refuses stored release values that still set it. Delete it
from your values files and upgrade once with `--set argoSync=null`; Helm's merge
drops a stored key that an override sets to null. The Oberth chart refuses to
upgrade while the live ConfigMap still belongs to the Oberth release without
`keep`, so it is never deleted by accident.

With the nonroot controller profile (`--argo-controller-profile`), the
installer puts the limits ConfigMap beside the profile's own ConfigMap in the
release's `extraObjects`; pass the profile flag on every run as before. Never
replace `extraObjects` with a list that lacks the profile ConfigMap: Helm
deletes it, and the controller (whose volume is `optional: false`) can no
longer start.

## Migrating from concurrency groups

`oberth.ci/concurrency-group` and `oberth.ci/concurrency-limit` are removed,
like `oberth.ci/exclusive`: admission and `oberth validate` refuse a document
that carries one, naming the replacement. A group of one is a
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
  Oberth's queue order; a release run (`spec.priority: 1`) is granted a
  shared lock before branch runs that waited longer.
- A Workflow waiting for a workflow-level mutex occupies an Argo parallelism
  slot ([above](#what-occupies-a-slot)); a parked group member did not.
- A scoped (non-admin) uplink may cancel a branch run that holds a mutex;
  concurrency-group members could be cancelled only by an admin. Release, tag,
  apply and scheduled runs stay admin-only.
- The `concurrency_groups` MCP tool and `oberth admission groups` are gone;
  `sync_status` reports mutex and semaphore holders, waiters and history.
