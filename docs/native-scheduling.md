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
refused. Authors may set Pod labels and annotations in `spec.podMetadata` and
`templates[].metadata` (including defaults/inline templates). Use literal
Kubernetes-qualified keys. `oberth.ci/` and `workflows.argoproj.io/` keys are
reserved, as are the installed selector keys `app.kubernetes.io/name`,
`app.kubernetes.io/instance`, `app.kubernetes.io/component`, `component`,
`openbao-active`, `openbao-internal`, and `kubernetes.io/metadata.name`; Argo’s
`kubectl.kubernetes.io/default-container` annotation is also reserved. This
protects the installed policies and Services without restricting native affinity
selectors. Existing validated template controls for workspace mounts/env and
release WIF remain author-facing configuration. Other author annotations pass
through; an operator’s additional admission webhooks remain cluster policy.
Oberth’s own run/trigger/tier labels remain server-assigned.
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
controller-profile pause banner. Restore the exact Ready controller, profile
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

## TEMPORARY DEVIATION: measurement executable architecture

Usage instrumentation wraps eligible steps with the server-delivered native
executable. Its server-owned Pod patch adds `kubernetes.io/arch` matching that
binary, after Argo resolves the authored Workflow/template placement. The
original fields are preserved, but the constructed Pod has this extra selector;
instrumentation is skipped when authored selectors or node affinity name a
different architecture; it never replaces those authored constraints. This violates the
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
and outside Argo parallelism. Source preparation waits while a seed Pod is
Pending, without creating the Workflow. Cancellation or supersession stops it
and cleans its Pod and PVC. There is no automatic infrastructure retry. A
manual retry attempts source
delivery again; YAML retryStrategy cannot cover a failure before Workflow Create.
Custody-bound execution targets retain their uncertainty/cleanup proof before
retrying. Cancellation, supersession and publication can affect retention.
This is unbounded source retention. Its native replacement is tracked separately.

## What Oberth writes into the user's Workflow and Pods

This inventory describes current submission (`internal/argojob/spec.go`,
`executor_tokens.go`, `nonroot.go`, `release_wif.go`, `step_usage.go`,
`sourcevolume.go` and `artifactcollect.go`). The metadata tables below list
individual server labels and annotations. Conditional features apply only when
configured/authorized. Oberth's injected environment and reserved mount paths
win name/path collisions; validated workspace opt-outs remain available.

- **Object identity and parameters.** Oberth assigns the Workflow name,
  namespace, run/repository/SHA/trigger/tier labels and immutable source,
  policy and custody annotations. Pod metadata adds run/trigger/tier labels;
  other author Pod metadata survives outside reserved keys. Declared trigger
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
  cause native admission/startup failure. The separate authenticated execution
  target's already documented placement remains an explicit exception.
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
  and inserts `oberth-wait-executor-token` before Argo's init. Oberth delivers
  a Pod-bound executor token over exec after verifying the exact Pod identity;
  the waiter exposes no token in a Kubernetes object. The init/wait/waiter
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
- **Usage instrumentation.** Eligible explicit container/script commands
  become `/run/oberth/usage/oberth internal-step-exec -- <authored command>`;
  authored args and Argo's script-path behavior remain. The executable is
  mounted read-only at `/run/oberth/usage` from the authenticated source-claim
  binary. Argo adds its own emissary entrypoint around this command later.
  Instrumentation uses the existing explicit-memory-limit safety check;
  opaque entrypoints/container sets or incompatible architecture placement
  remain uninstrumented, with usage unknown. The wrapper/mount add runtime
  work but **no resource request rewrite**. Instrumented Pod patches add
  `kubernetes.io/arch` for the delivered binary, **an extra placement
  constraint and the named architecture deviation**. An authored different
  architecture is never overwritten or narrowed. Instrumentation itself is
  unchanged in this round.
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

A template can additionally set its own labels/annotations, for example
`metadata.labels: {component.example.test/part: worker}`. Both scopes reach
the Pod. Selectors may also read the server-owned `oberth.ci/run` label or
`workflows.argoproj.io/workflow: WORKFLOW_NAME` for one Workflow. Use actual existing label values. A selector on `oberth.ci/repo` will
not select every user Pod of that repository: Oberth does not inject that label
onto those Pods.
