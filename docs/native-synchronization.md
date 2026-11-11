# Admission and native synchronization

Oberth admits runs by counting them. What a step needs is the step's own
`resources` block, placed by Kubernetes. What a step must not share with other
steps is native Argo `synchronization`, queued by the Argo workflow controller.
Oberth reads nothing from a workflow document to decide whether a run may
start, and it submits `synchronization` exactly as written.

## What Oberth keeps

Since v0.20.0 Oberth has no run ceiling and no concurrency groups: every limit
on what runs at once lives in Argo ([where limits live](where-limits-live.md)).

| Setting | Helm | Meaning |
| --- | --- | --- |
| Pressure valve | `admission.pressure.{enabled,hardCpu,hardIo}` | Holds new grants while host CPU or IO PSI is above the hard limit. Reads `/proc/pressure`, never a workflow. |
| Disk floor | `admission.diskFloorBytes` | Holds new grants while the data disk is below the floor. |
| Administrator hold | `admission_hold` | Keeps selected run classes queued. |
| Startup stall bound | `admission.stallTimeout` | Fails an admitted Workflow that neither schedules a Pod nor makes progress. Kubernetes capacity waits, Argo synchronization waits and Argo parallelism postponement are exempt. |

Priority (`low < standard < promotion < high`) orders Oberth's queue, which is
the order runs are submitted in.

## Shared step limits: semaphores

An operator-defined limit is a key of the ConfigMap `oberth-sync-limits` in the
pipeline namespace. Since v0.20.0 the argo-workflows release renders it
([where limits live](where-limits-live.md#shared-step-limits)):

```sh
oberth install --upgrade --argo-sync-limit kvm=1 --argo-sync-limit heavy-go=2
```

A step that must respect the limit declares the native Argo semaphore on its
template:

```yaml
templates:
  - name: kernel-matrix
    synchronization:
      semaphores:
        - configMapKeyRef:
            name: oberth-sync-limits
            key: kvm
    container:
      image: ...
```

At most `kvm` instances of any such step run at once, across every repository
and trigger. The Argo controller queues the rest in priority then creation
order and sets the node message `Waiting for <ns>/ConfigMap/oberth-sync-limits/kvm
lock. Lock status: 0/1`; Oberth shows it in the run's existing reason line as
`waiting for lock ConfigMap/oberth-sync-limits/kvm (0/1 free)`. No
status, label or layout is added.

Argo re-reads the ConfigMap, so an upgrade of the argo-workflows release that changes a limit applies
to the next acquisition without a server restart. Lowering a limit below the
current holders is tolerated: holders drain, no new instance starts.

Put the semaphore on the template that needs it, not on the workflow. A
template-level wait lets the rest of the run proceed and keeps the Argo
workflow deadline running (Argo anchors it to `startedAt`); a workflow-level
`spec.synchronization` holds the whole run before it starts and burns nothing.
Both forms pass through unchanged.

## One-at-a-time steps: mutexes

```yaml
templates:
  - name: publish-index
    synchronization:
      mutexes:
        - name: chart-index
    container: ...
```

A mutex is a semaphore of one that needs no ConfigMap. Names are submitted as
written: a CI pipeline and a release pipeline that name the same mutex share
it, which is what a compare-and-swap over a shared index needs. Nothing scopes
or rewrites the name any more.

## Serialized releases

`releases.serialize: true` (`--serialize-releases`) gives every release run of
every repository the workflow-level mutex `oberth-release`, so no two releases
run at the same time. The decision is the run's trigger, not its document; a
document's own `spec.synchronization` is kept beside the server's mutex. The
name `oberth-release` is reserved: admission refuses a document that declares
it at workflow or template level, so only the server holds it. A templated
mutex name must start with literal text that rules the reserved name out
(`deploy-{{workflow.parameters.env}}` is admitted, `{{workflow.parameters.lock}}`
is not), because Argo resolves the name only at run time. A
second release shows `waiting for lock Mutex/oberth-release (0/1 free)`
until the first finishes. Off by default. A release waiting for the mutex has
been admitted by Argo and occupies a controller parallelism slot
([what occupies a slot](where-limits-live.md#what-occupies-a-slot)).

## Waiting is not stalling

A Workflow held Pending behind a lock, or a step held Pending behind one, is
exempt from `admission.stallTimeout`, and the server's pre-start watchdog does
not count a workflow-level lock wait: Argo's own deadline begins at `startedAt`,
so neither does Oberth's. The required `activeDeadlineSeconds` still bounds the
run once it starts, and a step's `activeDeadlineSeconds` is a Pod deadline that
starts when the Pod runs.

## Retry hold

A held retry ([assisted continuation](assisted-continuation.md)) keeps its
template lock: Argo acquires a template-level lock once, for the retry parent,
before attempt 1, and releases it when the retry parent is fulfilled. The hold
parks the retry child; the lock stays taken for the hold. Size the semaphore
and `assist.maxConcurrentHolds` together: a held run on a limit-1 key keeps
that key for up to the step budget plus two `assist.holdTTL`.

## Security

- A semaphore reads one integer from a key of the `oberth-sync-limits`
  ConfigMap in the run's namespace, through the Argo controller's own RBAC
  (its single-namespace Role grants `configmaps` get/list/watch). Admission
  refuses any other ConfigMap, a `namespace` override and a `database`
  semaphore: the controller copies a value it cannot parse into the
  Workflow's status message, so another ConfigMap key would be readable
  through a run. No ConfigMap content reaches a Pod through
  `synchronization`.
- A mutex is a name. A repository that names another repository's lock delays
  it, bounded by its own `activeDeadlineSeconds`; the former shared
  concurrency groups allowed the same. The reason line names the lock so this
  is visible.
- The lock key in the reason line is admitted only when it matches the
  controller's lock-key grammar `<ns>/<ConfigMap|Mutex|Database>/<name>[/<key>]`
  over `[A-Za-z0-9_.-]`, and the namespace is dropped; anything else renders
  as `Waiting for lock (synchronization).`
- `ARGO_*` environment names are still refused from repository YAML. This is
  a content check kept for the retry hold's safety (the hold is driven by
  `ARGO_DEBUG_PAUSE_BEFORE`), not for scheduling.

## Migration from `oberth.ci/exclusive`

The per-step annotation `oberth.ci/exclusive: <class>`, the Helm
`admission.exclusiveClasses` allowlist and the in-process class ledger are
gone. The annotation is accepted and ignored, so a document that still carries
it runs without a limit. To keep the limit, replace it with the native
semaphore:

```yaml
# before
    - name: kernel-matrix
      metadata:
        annotations:
          oberth.ci/exclusive: kvm

# after
    - name: kernel-matrix
      synchronization:
        semaphores:
          - configMapKeyRef:
              name: oberth-sync-limits
              key: kvm
```

and define the limit once on the argo-workflows release
(`oberth install --upgrade --argo-sync-limit kvm=1`; see
[where limits live](where-limits-live.md#shared-step-limits)).

Also removed: `admission.sizeWeights`, `admission.declaredResources`,
`admission.maxHeadWait` and the server flags behind them. The historical
`oberth.ci/size` annotation is still accepted and still has no effect.
