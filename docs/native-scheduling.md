# Native scheduling

Oberth accepts a push, checks its Workflow for dangerous content, makes its
source available, and submits it to Argo. It has no resource admission queue.
Argo queues Workflows under its global parallelism and native synchronization;
Kubernetes queues Pods until their authored requests and placement fit.
Existing queued/running statuses and reason lines reflect those observations.
Supersede, branch deletion, cancellation, publication and recovery remain CI
lifecycle operations.

A Workflow can stop through `argo stop` or `argo terminate` on a live Workflow
or through `spec.shutdown` in its Workflow spec; Oberth reports
`Workflow stopped: <Argo message>` without attributing who initiated it.

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
refused. Authors may set Pod labels in `spec.podMetadata` and
`templates[].metadata` (including defaults/inline templates). Use literal
Kubernetes-qualified keys. `oberth.ci/` and `workflows.argoproj.io/` keys are
reserved, as are the installed selector keys `app.kubernetes.io/name`,
`app.kubernetes.io/instance`, `app.kubernetes.io/component`, `component`,
`openbao-active`, `openbao-internal`, and `kubernetes.io/metadata.name`; the exact Argo-owned
`kubectl.kubernetes.io/default-container` key is also reserved. This
protects the installed policies and Services without restricting native affinity
selectors. Repository YAML cannot set `priorityClassName`, `podPriorityClassName` or
`schedulerName`, including through Pod patches or nested templates. These
refusals guard against one repository pre-empting another's Pods or bypassing
the default scheduler. Native Workflow `spec.priority` remains available.

Author Pod annotations are not admitted: they can switch runtime, CNI and
injection-webhook behavior, including weakening security, and cannot be secured
with an enumerated deny-list. Existing validated template controls for workspace
mounts/env and release WIF remain author-facing configuration, as before this
change; there is no general author Pod-annotation allow-list.
Oberth’s own run/trigger/tier labels remain server-assigned.
The same Pod annotation and label rules apply to `artifactGC.podMetadata`,
including artifact-GC Pods created for archived logs. Artifact-GC Pod patches
remain refused. Hooks, exits and inline/default templates pass through the same
recursive template admission. Opaque granted resource manifests refuse nonempty
annotations and reserved or invalid labels only under object `metadata` or
`podMetadata`, as well as nested Pod patches. Granting an operator kind means
trusting how its controller maps fields to Pod metadata. The operator control is
a ValidatingAdmissionPolicy on the granted kind; see
[operator controls](workflow-capabilities.md#operator-controls-for-resource-templates).
Workflow-only metadata and PodGC label selectors do not write arbitrary Pod metadata.
An author's Pod label can match selectors in the operator's own policies
elsewhere. Operator policies should select on namespace as well as labels,
including NetworkPolicy rules that otherwise admit matching Pods from any
namespace. Oberth reserves the keys its own installed charts select on.
Affinity namespace selectors are explicitly permitted despite other namespace
reference restrictions.
Mutex and semaphore names are shared by all repositories in the pipeline
namespace, as Argo defines them. One repository can hold a lock that another
repository uses; Oberth imposes no repository prefix rule.
A repository semaphore may read only the `oberth-sync-limits` ConfigMap in the
run's namespace, using a key of the author's choice. Other ConfigMap references
remain refused because Argo's failure text can expose their data. Oberth no
longer creates or manages this ConfigMap. Upgrade preserves an existing object
and its data with Helm ownership removed; new installs create none. Operators
apply and maintain semaphore limits themselves.

An administrator-selected execution target still authenticates its approved
cluster, namespace, controller and service identity, but adds no nodeSelector or
tolerations and refuses no authored placement because of a target. Repository
YAML carries nodeSelector, affinity and tolerations. A dedicated node is guarded
by Kubernetes taints plus the admission or namespace policy the operator sets
up; Oberth does not guarantee that a target's runs land only on a particular
node. Ordinary repositories may author tolerations too, so taints alone do not
establish exclusive access to a trusted node. See [execution targets](execution-targets.md).
Extended resources, including GPUs, are native requests/limits: Oberth requires
no device grant and imposes no amount or aggregate bound. Operators use
Kubernetes ResourceQuota and admission policy in the pipeline namespace; see
[the quota example](workflow-capabilities.md#extended-resources). Dangerous
settings such as privilege, host paths/device mounts, host namespaces, runtime
classes, unapproved images or service identities, and `resources.claims` remain
refused independently of the requested resource.
Server-owned executor/identity helper containers keep their existing requests:
**50m CPU, 64Mi memory and 256Mi ephemeral storage**. The executor wait container
usually adds those quantities to the steady-state container sum. Kubernetes
calculates the effective Pod request using that sum and the applicable
init-container maxima; an authored init container can dominate the result.
For example, an ordinary init requesting 2 CPU and 1Gi memory can leave the
effective request at 2 CPU and 1Gi rather than adding the helper quantities.
A Pod whose author declares no requests is therefore **Burstable**. The author's
container resources are unchanged. Opted-in assisted holds retain their
private scratch resources, retry/hold behavior and hold-lifetime deadlines.

## Security waits before submission

Two security checks can delay Workflow submission without doing resource
scheduling. `awaitWorkflowPolicy` (`internal/service/workflow_policy.go`) keeps a
claimed run while refreshing the trusted upstream policy, for at most 30
minutes. The run reports `waiting for upstream policy`; its scheduling message
names the unavailable trusted policy, and publication is forbidden. Operators
restore upstream connectivity/credentials and the trusted policy source; an
expired wait fails with a policy-refresh deadline error and can be retried.

`awaitAdmission` (`internal/service/scheduler.go`) and the controller profile
retry (`internal/argojob/profile_retry.go`) wait without a deadline while the
required immutable controller/profile proof is unverified. This can pause
dispatch or a claimed run before Create. Operators see `controller_profile`
state/reason/remedy and retry timing in status and the dashboard's existing
"Controller verification required" banner with `dispatch_blocked`. Restore the exact Ready controller, profile
ConfigMap, images and required read permissions named by that diagnostic.
Existing execution observation and cleanup retain their authority. These waits
protect policy and identity; neither examines CPU, memory or node capacity.
Cancellation remains a lifecycle action, not a placement decision.

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
delivery deviation, not a native placement guarantee. Authenticated execution
targets also add no seed nodeSelector or tolerations.

With N active runs and Argo parallelism P, all N prepare and submit; P bounds
neither server copies nor PVCs nor the source-seed Pods. Up to N seed Pods and N
volumes can be created concurrently. Seed Pods run before their Workflow exists
and outside Argo parallelism. Source preparation waits while a seed Pod is
Pending, without creating the Workflow. A deleted seed Pod, or one that ends
Failed/Succeeded before preparation, fails this run with a source-seed reason
and cleans its claim; it does not leave a permanent Running run. Image pull
back-off and an unbound claim keep waiting without an Oberth expiry. Run
reasons describe the exact seed Pod’s curated Kubernetes wait before a
Workflow exists. Cancellation or supersession stops it
and cleans its Pod and PVC. There is no automatic infrastructure retry. A
manual retry attempts source
delivery again; YAML retryStrategy cannot cover a failure before Workflow Create.
Custody-bound execution targets retain their uncertainty/cleanup proof before
retrying. Cancellation, supersession and publication can affect retention.
This is unbounded source retention. Its native replacement is tracked separately.

## What Oberth writes into the user's Workflow and Pods

This inventory describes current submission (`internal/argojob/spec.go`,
`executor_tokens.go`, `nonroot.go`, `release_wif.go`, `assist_hold.go`,
`sourcevolume.go` and `artifactcollect.go`). The metadata tables below list
individual server labels and annotations. Conditional features apply only when
configured/authorized. Oberth's injected environment and reserved mount paths
win name/path collisions; validated workspace opt-outs remain available.

- **Object identity and parameters.** Oberth assigns the Workflow name,
  namespace, run/repository/SHA/trigger/tier labels and immutable source,
  policy and custody annotations. Pod metadata adds run/trigger/tier labels;
  other author Pod labels survive outside reserved keys; author Pod annotations
  are refused. Declared trigger
  and schedule arguments are bound to the actual run. These do not set node
  placement or resource requests. Labels can affect cluster policy and native
  affinity when selected; that is why installed selector keys are reserved.
- **Environment.** Run containers receive `OBERTH_REPO`, `OBERTH_REF`,
  `OBERTH_SHA`, `OBERTH_TRIGGER`, `OBERTH_RUN_ID` and per-template `OBERTH_STEP`.
  Artifact delivery adds `OBERTH_ARTIFACTS`. Release runs add
  `OBERTH_RELEASE_TAG` and `OBERTH_RELEASE_SHA`. Standard work-volume setup
  supplies `PATH`, `OBERTH_TOOLS_DIR`, `HOME`, `GOPATH`, `GOTOOLCHAIN`, `GOWORK`,
  `GOENV`, `GOMODCACHE`, `GOCACHE`, `GIT_TERMINAL_PROMPT`, `KUBEBUILDER_ASSETS`,
  `TRIVY_CACHE_DIR` and `GOLANGCI_LINT_CACHE`; workspace annotations can omit
  these defaults. A configured persistent cache overrides `GOMODCACHE` and
  `GOCACHE` and adds `OBERTH_CACHE_DIR`, pointing to that cache instead of the
  work-volume fallbacks. A configured private module proxy sets `GOPROXY`,
  `GONOPROXY` and `GONOSUMDB`, including the authored mirror-only option.
  Credentialed leaves receive `VAULT_ADDR`, `OBERTH_VAULT_ROLE` and, when
  delivered, `VAULT_CACERT`; approved per-repository/per-step identities
  specialize the role. Approved release WIF adds
  `GOOGLE_APPLICATION_CREDENTIALS` and `OBERTH_RELEASE_WIF`. The nonroot profile
  additionally sets `TMPDIR`, `GOTMPDIR`, `XDG_CACHE_HOME`, `HOME` and private
  scratch Go/cache paths. Environment affects program behavior and I/O,
  **not scheduler placement or declared resource quantities**.
- **Security context and identity.** The ordinary Pod baseline uses UID/GID 0,
  `runAsNonRoot: false` and RuntimeDefault seccomp. Main/script/container-set,
  authored init containers and sidecars get no privilege escalation, all
  capabilities dropped and a read-only root filesystem; conflicting template
  security contexts are cleared. The verified nonroot profile instead uses
  UID/GID 65534 and private scratch for selected leaves, including patched
  Argo init/wait containers and a fixed root initializer for fresh EmptyDirs.
  Oberth forces the pipeline, CI-secrets, release or approved per-step
  ServiceAccount and the separate executor ServiceAccount. Ordinary leaves
  have no automounted API token; authorized credential leaves receive only
  their scoped token mounts. These are security/startup compatibility rules,
  **not placement policy**; missing identities or incompatible security can
  cause native admission/startup failure. Authenticated execution targets retain
  cluster, namespace and identity custody without a placement exception.
- **Scratch and work volumes.** Oberth adds a bounded disk EmptyDir at `/tmp`
  (8 GiB size limit) to ordinary containers; the nonroot profile uses its own
  private `/tmp` EmptyDir and preparation mounts. If the author declares a
  `work` volumeClaimTemplate, Oberth adds its tools, fallback Go caches, Trivy
  cache and release-artifact subpath mounts at `/tmp/oberth-tools`,
  `/cache/gomod`, `/cache/gobuild`, `/tmp/oberth-trivy` and, for release,
  `/tmp/oberth-release`. Workspace annotations can omit these or make shared
  mounts read-only. EmptyDir size limits constrain storage usage, not node
  selection; they are not ephemeral-storage requests. PVC binding and access
  modes **can constrain placement**. No new work PVC is invented when the
  document lacks that volumeClaimTemplate.
- **Persistent cache.** When configured, a writable, repository-scoped,
  tier-separated `DirectoryOrCreate` hostPath is mounted at `/work/cache`.
  This is server-owned; repository hostPath declarations remain refused.
  It changes node-local storage locality/compatibility and runtime I/O but
  **adds no cache node selector or compute request**.
- **Source, artifacts and public inputs.** The run's source PVC is mounted
  read-only at `/work/src`; an omitted working directory defaults there.
  Approved pinned sources get additional read-only subpaths at their declared
  paths. `/work/artifacts` is a writable subpath of the same claim when
  collected artifacts are enabled. Public Vault/proxy trust anchors and the
  authenticated server binary are additional read-only source-claim subpaths
  where required (`/run/oberth/vault-ca`, `/etc/ssl/certs/oberth-goproxy-ca.pem`,
  `/run/oberth/bin`). The claim is already bound by source seeding:
  **its RWO/node/storage topology constrains subsequent Pods**, including
  collection. The source deviation above describes the extra pre-Workflow
  server copies, PVC and seed Pod; these remain unchanged and unbounded.
- **Executor-token init container and helper resources.** A server patch
  replaces Argo's executor Secret volume with a bounded 1 MiB memory EmptyDir
  and inserts `oberth-executor-token` before Argo's init. Oberth delivers
  a Pod-bound executor token over exec after verifying the exact Pod identity;
  the waiter exposes no token in a Kubernetes object. The token is written
  under a random 128-bit directory name, fresh per delivery, never at the
  in-cluster default `token` path; the unguessable name is the protection (a
  root executor with default capabilities can still list the directory, and
  nothing lists it today because artifacts, data templates and `archiveLocation`
  are refused), and the patch sets `KUBECONFIG` on Argo's init and wait
  containers (and the main container of resource and data templates) to the
  delivered kubeconfig that names it (#1564). The same patch sets both Argo
  progress ticks to `0` there, so Argo's self-reported step progress (`N/M`)
  is disabled: its monitor would otherwise call the API, and re-read the
  token, while wait reads a step's outputs. The init/wait/waiter
  helpers normally request 50m CPU and 64 MiB memory, with 500m/512 MiB limits.
  With helper-storage accounting they also request 256 MiB ephemeral storage;
  the 512 MiB helper storage limit is applied only when the step itself
  declares a storage limit. The nonroot preparation helper requests 10m CPU/16 MiB memory and has
  100m/64 MiB limits. **Helper requests affect Kubernetes Pod packing**
  (init/steady-state aggregation), and token delivery is a security startup
  dependency. They do not fill or resize the author's container requests.
  The separate token-identity holder Pod deliberately carries a scheduling
  gate and never executes; it has its own ServiceAccount, 1m/1 MiB requests
  and 10m/8 MiB limits, and is retired with its owned Workflow identity.

  Generated helper container names (including Argo's patched executors):

  | Container name | Role |
  | --- | --- |
  | `init` | Argo executor initialization |
  | `wait` | Argo executor wait container |
  | `oberth-executor-token` | Pod-bound executor-token delivery waiter |
  | `oberth-prepare-nonroot` | Preparation for an opted-in nonroot template |

- **Post-run collect Pod.** When collecting artifacts, Oberth creates a
  helper outside the Workflow using the source-seed Pod shape,
  mounting the same source PVC at `/seed`, and streams the artifact archive
  back before deleting that Pod. The seed and collect helper request 100m
  CPU, 64 MiB memory and 16 MiB ephemeral storage, with 500m/512 MiB/64 MiB
  limits. They keep the helper PriorityClass and `preemptionPolicy: Never`.
  **Their resources and source-volume node binding affect scheduling**.
  They do not inherit the ordinary Workflow's authored affinity/selectors;
  collection is tied to the node/topology of that source volume. The running
  helper's `sleep 900` is a leak backstop, not a Pending-Pod expiry. These
  separate helpers run outside Argo's Workflow parallelism.
- **Step measurement removed.** Oberth adds no measurement command wrapper,
  usage executable mount, memory-policy/warning annotations or architecture
  selector. Authored commands and args pass through unchanged by measurement.
  Per-step CPU/memory/storage usage figures, the external sampler and
  run_resources/repo_usage reports are removed. The static YAML resource guard
  remains. Existing secret-store execution and explicitly opted-in assisted
  filesystem helpers retain their separate authenticated executable delivery;
  assisted helper mounts use /run/oberth/assist and add no architecture selector.
  Those helper executables are native binaries; authors/operators must choose
  compatible execution environments if they use those features. They no longer
  cause Oberth to select a worker architecture.
- **Retention and native fields.** Oberth writes `ttlStrategy` with
  `secondsAfterCompletion` from configured retention (at least 180 seconds)
  and removes authored PodGC so step logs remain for the Workflow's retention
  window. **These clean up completed work, not the live scheduling queue**.
  Authorized assisted continuation separately retains its documented retry,
  hold-lifetime and scratch behavior. Ordinary authored resource requests,
  affinity, node selectors, tolerations, parallelism, priority,
  synchronization, suspend, retryStrategy and optional deadlines pass through;
  no additional Oberth capacity queue, default deadline or release mutex is
  introduced.

A branch receive records the accepted ref and queued run before this full
Workflow validation. Validation happens against the checked-out, resolved
source receipt before Create. A deterministic decode/admission refusal fails
that run in the existing `job` phase; `error` retains the complete rule and the
bounded display reason says `validation refused: …`. Its retry hint asks for a
YAML fix and new commit. Genuine source/API I/O failures remain infrastructure
failures. Accepted event-ID replay stays bound to the same run.

Suspension is admitted as vanilla Argo. Resume a run with the Argo CLI
(`argo resume WORKFLOW -n PIPELINE_NAMESPACE`), or use kubectl to update the
Workflow's suspension state. Canceling the run or pushing a newer revision
ends the suspended run. A suspended Workflow continues to hold one global
Argo slot until it resumes or ends.

Suspension reasons come from the live Workflow's `spec.suspend` or an active
Argo suspend node. Controller-parallelism waits likewise come from native Argo
state. Run lists observe these even before local step records exist and after
steps start; fast reads appear in the first response, with a single 100 ms
page-wide read grace. A slow API retains an aged observation or says
`awaiting Argo state`. This read budget neither starts nor fails a workload.

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
| `oberth.ci/workflow-capability` | Workflow, when granted | Opaque operator API-kind/action capability binding |
| `oberth.ci/helper-storage` | Workflow | Versioned server-helper footprint binding |
| `oberth.ci/verified-controller-profile` | Workflow, nonroot profile | Opaque verified controller identity/proof |
| `oberth.ci/execution-placement` | Workflow, approved target | Approved execution authority digest; no node placement policy |
| `oberth.ci/execution-target` | Custody-tracked target objects | Exact durable target binding digest |
| `oberth.ci/release-wif-policy-sha256` | Workflow, WIF release | Public role/policy binding digest |
| `oberth.ci/release-wif-adc` | WIF template/Pod | Public ADC identity descriptor; no token |
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

For example, keep replicas of an authored workload apart using a label the
author sets on every Pod (the Kubernetes anti-affinity example):

```yaml
spec:
  podMetadata:
    labels:
      app: store
  affinity:
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              app: store
          topologyKey: kubernetes.io/hostname
```

A template can additionally set its own labels, for example
`metadata.labels: {component.example.test/part: worker}`. Both label scopes reach
the Pod. Selectors may also read the server-owned `oberth.ci/run` label or
`workflows.argoproj.io/workflow: WORKFLOW_NAME` for one Workflow. Use actual existing label values. A selector on `oberth.ci/repo` will
not select every user Pod of that repository: Oberth does not inject that label
onto those Pods.


Extended resources may also be requested by steps of credentialed and release
Workflows.

A Kubernetes ResourceQuota applies to the whole pipeline namespace and cannot
separate repositories within that namespace.

Granted resource manifests are checked recursively, including nested templates
and list entries. Nonempty maps named `annotations` and reserved or invalid
`labels` are refused only when their parent key is `metadata` or `podMetadata`,
including case-variant spellings. Custom fields such as
`spec.secretTemplate.annotations`, `serviceAnnotations`, PrometheusRule
`rules[].labels`, `externalLabels` and direct `podAnnotations`/`podLabels` pass
through unchanged. Custom label fields may use keys and values that are not
Kubernetes Pod labels. Parent names containing "pod" or "template" do not define
object metadata. Native `matchLabels` selectors and metric label lists remain
admitted.

Granting an operator kind means trusting how its controller maps fields to Pod
metadata. Enforce the operator's constraints with a ValidatingAdmissionPolicy on
the granted kind; see
[operator controls](workflow-capabilities.md#operator-controls-for-resource-templates).

Block PVCs use ordinary Kubernetes fields: a container may declare
`volumeDevices` backed by a per-Workflow `volumeClaimTemplates` entry with
`volumeMode: Block`. Admission preserves the device path and claim mode.
This does not grant host device access or permission to mount an existing PVC;
those volume sources remain refused. Device paths follow the same absolute-path
and reserved-helper-path rules as volume mounts.
