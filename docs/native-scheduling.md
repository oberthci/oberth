# Native scheduling

Oberth accepts a push, checks its Workflow for dangerous content, makes its
source available, and submits it to Argo. It has no resource admission queue.
Argo queues Workflows under its global parallelism and native synchronization;
Kubernetes queues Pods until their authored requests and placement fit.
Existing queued/running statuses and reason lines reflect those observations.
Supersede, branch deletion, cancellation, publication and recovery remain CI
lifecycle operations.

## Repository authors

Use ordinary Argo Workflow YAML: container, script, containerSet, init-container
and sidecar `resources`; Workflow and template `affinity`, `nodeSelector` and
`tolerations`; `parallelism`, `synchronization`, `spec.priority`, deadlines and
`retryStrategy`. Required/preferred node/pod affinity and anti-affinity accept
any label selector and topology key, `namespaces`, `namespaceSelector`,
`matchLabelKeys` and `mismatchLabelKeys`. Oberth adds no selector restriction.
The pinned Argo v4.0.8 Workflow/Template schema does not expose
`topologySpreadConstraints`; arbitrary `podSpecPatch` remains refused.

Oberth preserves these authored fields. It does not fill missing requests,
measure and resize requests, cap compute declarations, inject parallelism, or
supply a default/capped Workflow deadline. Missing request dimensions count as
zero in Kubernetes packing; Kubernetes itself can default a request from a
limit, and a namespace LimitRange can supply defaults. No request or deadline
is mandatory in Oberth. A Pod fitting no node remains Pending until native
policy, its authored deadline, or cancellation ends it. Without a deadline, a
Workflow whose Pod can never be scheduled holds one global Argo slot until
someone cancels it.

The security boundary stays: reserved executor environment and ownership
metadata; privilege/escalation/capabilities; host namespaces, hostPath and host
ports; token/ServiceAccount/automount overrides; unauthorized secrets/configs,
volumes, images, registries, runtimes and devices; and arbitrary Pod patches
remain unavailable. Workflow identity is assigned by the server; template and
patch identity escape routes are refused. Repository Pod priority classes stay
refused. Authored Pod metadata remains restricted because labels/annotations
can select NetworkPolicies or trigger security-sensitive admission mutation.
Affinity namespace selectors are explicitly permitted despite other namespace
reference restrictions.
A repository semaphore may read only the `oberth-sync-limits` ConfigMap in the
run's namespace, using a key of the author's choice. Other ConfigMap references
remain refused because Argo's failure text can expose their data. Oberth no
longer creates or manages this ConfigMap; operators apply and maintain it.

Two existing security exceptions remain explicit: an administrator-selected
execution target must still prove its exact approved namespace/node/controller
placement and rejects conflicting source placement; granted device resources
still require a bounded aggregate device authorization proof. Neither is a
CPU/memory capacity scheduler. Ordinary Workflows do not use either exception.
Server-owned executor/identity helpers have their own requests; these do not
rewrite the author's container resources. Opted-in assisted holds retain their
private scratch resources, retry/hold behavior and hold-lifetime deadlines.

## Operators

Set the Argo controller limit with `oberth install --argo-parallelism N`.
Existing values are preserved unless explicitly changed. A fresh install keeps
the existing default: half observed allocatable CPU, minimum 2, capped at the
installer's maximum; if node CPU cannot be read, no default limit is installed.
`0` means unlimited. It never means paused.

Pause natively with `kubectl -n oberth-argo scale deployment
argo-workflows-workflow-controller --replicas=0`, or `argo -n oberth-argo
suspend WORKFLOW` for one Workflow. Resume the controller at its previous
replica count or use `argo resume`. Suspending does not stop Pods already
running. No Oberth admin hold, disk floor, PSI valve or startup stall timeout
holds runs. A full server disk causes work to fail instead of pausing a queue.

Operators can use Kubernetes ResourceQuota scoped to `CrossNamespacePodAffinity`
on the pipeline namespace and the `PodTolerationRestriction` admission plugin.
Oberth implements neither control. Native ResourceQuota, LimitRange, admission
policies and node capacity remain cluster policy.

The installer keeps installing upstream Argo and, optionally, the bundled
secret store. Their upstream chart requests/classes apply; Oberth no longer
sets 100m/128Mi requests or its control-plane class on either. A secret-store
StatefulSet using OnDelete picks up this change only when its Pod is recreated.
Oberth's own server Deployment keeps its control-plane class.

The pipeline PriorityClass object remains for Workflows submitted by v0.21.10:
their later Pods still reference it. New user Workflows do not receive that
class. Source/executor helper Pods still use it. The object can be removed in
the release after those old Workflows end.

Oberth has no automatic infrastructure retry. Authors choose native Argo
`retryStrategy`, such as `retryPolicy: OnError`, on Workflow/templates. Manual
`run_retry` and `run_rerun` remain. Argo cannot retry a preparation failure that
happens before a Workflow exists; retry that failed run manually.

## TEMPORARY DEVIATION: measurement executable architecture

Usage instrumentation wraps eligible steps with the server-delivered native
executable. Its server-owned Pod patch adds `kubernetes.io/arch` matching that
binary, after Argo resolves the authored Workflow/template placement. The
original fields are preserved, but the constructed Pod has this extra selector;
a conflicting authored architecture can remain Pending. This violates the
no-placement-injection model and remains a second explicit deviation.

The release publishes Linux amd64 and arm64 executables, but each server image
contains only its own executable. Source delivery copies `os.Executable()` and
verifies its SHA-256 after streaming to the seed Pod. Removing this selector
requires changing the server image's contents and release delivery: include both
authenticated executables, verify each delivered hash, select the matching
binary inside the step, and cover both architectures and recovery. That is
outside this bootstrap correction. The selector remains until that delivery
replacement is implemented; execution-target placement is unchanged.

## Source delivery and load

Source delivery is a **TEMPORARY DEVIATION** from the native scheduling model,
unchanged until separately tracked native source delivery replaces it. Every active accepted run gets a
full checkout, including its own `.git`, on server disk and a source PVC in the
cluster. These copies remain while the Workflow waits in Argo, through its
terminal state and potentially through publication/cleanup. Nothing in Oberth
limits their number. Promotion workspaces can retain an additional server
checkout. Pinned-source exports add server data through submission.
Git-cache locks remain solely for correctness.
The ordinary seed Pod does not inherit the Workflow's nodeSelector, affinity
or tolerations. It mounts and binds a ReadWriteOnce PVC on whichever node
schedules it. On a multi-node cluster that source volume ties the run's later
Pods to the seed's node/storage topology; authored placement excluding that
node stays Pending. This additional placement effect is part of the source
delivery deviation, not a native placement guarantee. The separate authenticated
execution target keeps its existing seed placement unchanged.

With N active runs and Argo parallelism P, all N prepare and submit; P bounds
neither server copies nor PVCs nor the source-seed Pods. Up to N seed Pods and N
volumes can be created concurrently. Seed Pods run before their Workflow exists
and outside Argo parallelism. A seed Pod that cannot start within the existing
source preparation waits while a seed Pod is Pending, without creating the
Workflow. Cancellation or supersession stops it and cleans its Pod and PVC. There is no automatic infrastructure retry. A manual retry attempts source
delivery again; YAML retryStrategy cannot cover a failure before Workflow Create.
Custody-bound execution targets retain their uncertainty/cleanup proof before
retrying. Cancellation, supersession and publication can affect retention.
This is unbounded source retention. Its native replacement is tracked separately.

## Labels and annotations

Label keys/meanings below are public selectors. Values are per-run identities
or trust classifications, not cross-install identifiers. `labelValue` sanitizes
and truncates Git/repository/run values to Kubernetes label limits; annotations
carry the complete identity. Policy/digest annotation values are opaque,
versioned implementation bindings: do not parse or select on their contents.
No new label was added for this release.

| Label | Objects Oberth sets it on | Meaning/stability |
| --- | --- | --- |
| `app.kubernetes.io/name=oberth-run` | Workflow | Stable workload kind |
| `oberth.ci/engine=argo` | Workflow | Stable engine kind |
| `oberth.ci/repo` | Workflow | Sanitized repository name; **not injected on user Pods** |
| `oberth.ci/sha` | Workflow | Sanitized tested SHA; not injected on user Pods |
| `oberth.ci/run` | Workflow and user Pods via `spec.podMetadata` | Sanitized run ID, stable for that run |
| `oberth.ci/trigger` | Workflow and user Pods | `ci` / `release` trust-facing pipeline trigger |
| `oberth.ci/tier` | Workflow and user Pods | Server-selected trust tier used by network/identity policy |
| `oberth.ci/role=executor-identity` | Executor identity holder Pod | Internal helper role; not a user Pod |
| `oberth.ci/component=source-seed` | Source seed Pod | Internal helper role; no Workflow exists yet |
| `oberth.ci/component=source` | Source PVC | Internal source custody marker |
| `oberth.ci/workflow` | Source PVC | Exact Workflow name for cleanup |

| Annotation Oberth writes | Scope | Meaning/stability |
| --- | --- | --- |
| `oberth.ci/run-id` | Workflow | Full immutable run ID |
| `oberth.ci/ref` | Workflow | Full Git ref |
| `oberth.ci/spec-identity` | Workflow | Opaque digest of intended submitted spec |
| `oberth.ci/declared-secret-paths` | Workflow, when credentialed | Admitted secret-path record; no values |
| `oberth.ci/sources` | Workflow with pinned sources | Canonical immutable pinned-source record |
| `oberth.ci/fragments` | Workflow, when fragments used | Immutable fragment-resolution record |
| `oberth.ci/workflow-capability` | Workflow, when granted | Opaque operator device/resource capability binding |
| `oberth.ci/helper-storage` | Workflow | Versioned server-helper footprint binding |
| `oberth.ci/verified-controller-profile` | Workflow, nonroot profile | Opaque verified controller identity/proof |
| `oberth.ci/execution-placement` | Workflow, approved target | Approved placement digest |
| `oberth.ci/execution-target` | Custody-tracked target objects | Exact durable target binding digest |
| `oberth.ci/release-wif-policy-sha256` | Workflow, WIF release | Public role/policy binding digest |
| `oberth.ci/release-wif-adc` | WIF template/Pod | Public ADC identity descriptor; no token |
| `oberth.ci/step-usage-memory-policy` | Workflow | Instrumentation memory-policy version |
| `oberth.ci/step-usage-memory-warning` | Template/Pod | Instrumentation unavailable reason; resources unchanged |
| `oberth.ci/assist` | Assisted Workflow | Hold TTL in seconds |
| `oberth.ci/assist-hold` | Assisted template/Pod | Held-attempt deadline binding |
| `oberth.ci/executor-identity-retry-at` | Owned step Pod on identity retry | Issuance retry timestamp, not a workload-start deadline |
| `workflows.argoproj.io/pod-name-format` | Workflow | Pinned Argo Pod naming format `v2` |

Author-facing annotations for source/workspace declarations, secret paths,
Go-proxy choice, nonroot templates and release-WIF roles remain documented in
their respective feature documents; they are not new scheduling syntax.
Removed CPU/request-default/demand/startup-allowance annotations are not emitted
and do not restore admission on old running Workflows.

Argo itself adds Pod labels `workflows.argoproj.io/workflow` (Workflow name),
`workflows.argoproj.io/completed` (`false`, later `true`), optional
`workflows.argoproj.io/on-exit` and controller-instance labels. It adds node-name
and node-id annotations, the default-container annotation, and optional Pod-GC
and progress annotations. These belong to the pinned Argo release, not Oberth.
Do not depend on generated Pod names or internal node IDs as stable selectors.

For example, keep Pods of one existing run apart using a native selector:

```yaml
spec:
  affinity:
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              oberth.ci/run: RUN_LABEL_VALUE
          topologyKey: kubernetes.io/hostname
```

Alternatively select `workflows.argoproj.io/workflow: WORKFLOW_NAME` for one
Workflow. Use actual existing label values. A selector on `oberth.ci/repo` will
not select every user Pod of that repository: Oberth does not inject that label
onto those Pods.
