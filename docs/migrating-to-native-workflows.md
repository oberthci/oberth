# Migrating to native workflows

Read the operator section before changing the server, then apply the repository
changes for that release after deployment. Examples are YAML excerpts; retain
the surrounding Workflow and substitute installation-specific values and images.

This guide uses the merged v0.22.7 documentation base, including authored
security contexts and credential-output guards. The administration API cutover
and executor-token follow-up still await merge; their blocks carry pending
comments. All v0.22.8, v0.23.0 and v0.23.1 sections are **DRAFT**, including
every recipe and quoted future refusal. They describe intended changes under
review, not an available upgrade. Do not apply a later recipe to an older server.

## Operators

### Requirements and backups

The chart requires Kubernetes 1.33 or newer. Managed Argo uses chart 1.0.24,
qualified with Argo v4.0.8 and the installer's pinned executor image. A controller
must watch the pipeline namespace. Keep the server and pipeline namespaces
distinct. Read-only planning does not establish current NetworkPolicy
enforcement; installation measures it after approval.

Keep both native task-result and executor-holder admission policies and their
Deny bindings. Both are present in the documentation base. Runtime does not check
that they still exist; removing either policy or its binding removes the
corresponding protection.

<!-- PENDING MERGE -->

The executor-token follow-up adds task-result DELETE protection against the
stored object's Workflow and retires every Workflow-owned identity holder,
including old names and capability accounts. It also requires wait's native
`lifecycle.postStart.sleep.seconds: 1`. Each step Pod's main container and
sidecars start about one second later, adding about one second per step. The
report proves admission on Kubernetes 1.33.1. Use normal cascading Workflow
deletion or Argo TTL; orphan deletion can hang on a holder owner update.

<!-- PENDING: confirm the exact native sleep lifecycle feature-gate requirement;
the token-hardening report proves admission on 1.33.1 but states no gate fact. -->

The margin reduces a shared-template startup race without closing it. Every
container of a step receiving credentials shares that step's trust. Neither
output guards nor the margin create an isolation boundary within that Pod.

Before each upgrade, keep the history archive and its decryption key, operator
values, and any separate OpenBao or server-volume data backups needed for
recovery. A history-only backup excludes secret values and log/artifact bytes.
Forward migrations do not provide a schema downgrade. Inspect a backup before
restore and review `oberth restore --dry-run`. Across chart generations, run
`oberth upgrade` first; reading an archive does not establish cross-generation
restore support.

### v0.22.7: before and after upgrade

<!-- PENDING MERGE -->

**One-time administration API cutover:** a v0.22.6 or older server has no
administration API. Before deploying the API-based CLI/server, use the verified
CLI matching the running version, then use the new CLI:

```sh
./oberth-v0.22.6 backup --history-only --context CONTEXT --namespace oberth
oberth upgrade --no-backup --context CONTEXT --namespace oberth --wait-idle --argo-security-defaults
```

Use the actual running version and selected cluster, not necessarily the example
version. Keep that archive for manual recovery: `--no-backup` does not record it
as the upgrade's automatic backup. Stop new submissions while draining.
A version-changing v0.22.8 upgrade waits by default; `--wait-for=30m` changes
the deadline.
This wait does not freeze admission. The API cutover observes native Workflows
through Kubernetes on older servers; if that observation is unavailable,
independently confirm idle before deliberately using `--no-wait-idle`. The consent
flag fills absent Argo security defaults; interactive review approval is the
alternative.

For the example version, context and namespace, the refusal prints:

```text
running release v0.22.6 has no administration API; pre-upgrade backup refused before any change. Use the verified v0.22.6 CLI saved as oberth-v0.22.6, then run:

  ./oberth-v0.22.6 backup --history-only --context CONTEXT --namespace oberth
  oberth upgrade --no-backup --context CONTEXT --namespace oberth

The retry requires this machine's client connected to this server as an unscoped admin.

If Kubernetes Workflow observation is unavailable, confirm no runs are active before retrying with --no-wait-idle. No exec fallback is used. Reapply your other upgrade options: --openbao-namespace, --values/--set, --backup-dir, --timeout, TLS flags, chart/release-source flags and downgrade consent are not copied into this retry command.
```

The command printer retains selected idle/security flags when supplied. S8 also
moves later admin operations to authenticated APIs; first-admin enrollment keeps
one install-time exec, recoverable using the saved enrollment identity. Interrupted installations resume with their saved enrollment identity.

Before upgrade:

1. Drain Workflows, including exit handlers. Executor identity changes can make
   old in-flight steps fail task-result writes. Re-run affected work if draining
   was skipped. This behavior is reported by the unmerged executor follow-up.
2. Review live Argo security defaults. On an existing managed Argo release,
   consent in the review or use `--argo-security-defaults` to fill absent values.
   Correct incompatible operator values yourself. External ConfigMaps require
   live native configuration; ordinary Helm values may not reach them.
3. Keep the Pod security policy enabled unless you intentionally manage that
   policy elsewhere. Defaults are still required with
   `argo.podPolicy.enabled=false`.
4. Remove execution-target, Go proxy and fragment settings from scripts and
   override files. Upgrade cleans retired stored settings. Reinstall/restore
   encountering old Go proxy settings require upgrade first.
5. Inventory unsupported refs using the read-only command in
   [run facts](run-facts.md). Rename them upstream before refresh; an unsupported
   default branch also prevents refresh and runs.

Before upgrading, record the count of interrupted runs with an outstanding
cancellation that the native Stop migration will resume observing. Run this
read-only query against a pre-upgrade database copy; it makes no changes:

```sql
SELECT COUNT(*) AS interrupted_with_pending_cancellation
FROM runs AS r
JOIN run_cancellations AS c ON c.run_id = r.id
WHERE r.status = 'interrupted'
  AND c.completed_at IS NULL
  AND c.job_name != ''
  AND c.reason != 'owner_restart'
  AND (r.trigger != 'promotion' OR NOT EXISTS (
    SELECT 1 FROM promotions AS p
    WHERE p.run_id = r.id AND p.status != 'pending'
  ));
```

For managed Argo, the safe native defaults have this shape (Argo values, not
repository Workflow YAML). Preserve unrelated operator fields:

```yaml
mainContainer:
  securityContext:
    privileged: false
    allowPrivilegeEscalation: false
    capabilities: {drop: [ALL]}
    seccompProfile: {type: RuntimeDefault}
executor:
  securityContext:
    privileged: false
    allowPrivilegeEscalation: false
    capabilities: {drop: [ALL]}
    seccompProfile: {type: RuntimeDefault}
```

The actual policy permits Localhost seccomp and NET_BIND_SERVICE as the sole
added capability. Root and writable root filesystems are allowed by that floor.
Check the effective merged Pod/template settings, not just this values excerpt.
The pipeline namespace remains privileged while hostPath caches exist; do not
raise it to baseline yet.

After deploying v0.22.7, update pipelines that used
`oberth.ci/nonroot-templates`: remove the annotation, author security contexts
and scratch volumes, and validate with the new CLI before the next run. Apply
run-fact mappings too. Re-approve exact grants when workflow files change. A
shared pipeline repository must also update its own affected workflows after
deployment. Check a plain run, a granted run and ordinary Stop cancellation. If
coming from before v0.22.5, **all secret approvals are cleared; re-approve**, then run
`oberth secretstore sync` and provision scoped identities. That is the earlier
v0.22.5 migration, not a fresh clearing on every upgrade.
The release also retires `argo.executorScratchSizeLimit` and its CLI flag.
Install/upgrade/restore cleanup drops that exact stored leaf, including old
coalesced defaults, while preserving operator settings. Argo owns executor scratch.

### DRAFT v0.22.8: watch, history and cleanup cutover

From v0.22.8, a version-changing `oberth upgrade` waits for idle by default,
with a visible countdown and a bounded timeout that refuses rather than
proceeding. `--no-wait-idle` skips that wait. This is an owner-decided DRAFT
change, not v0.22.7 behavior. Stop submissions while draining; the wait does not
freeze admission.

Before upgrade, back up history and establish the Argo native cleanup default
before the server stops injecting per-Workflow TTLs. The reported installer
fills only an absent `controller.workflowDefaults.spec.ttlStrategy`:

```yaml
controller:
  workflowDefaults:
    spec:
      ttlStrategy:
        secondsAfterCompletion: 3600
```

Explicit null, empty, zero and custom values are retained. External controllers,
skipped Argo reconciliation and server-only chart upgrades must configure their
own defaults first. Review authored PodGC and TTLs against log capture time.
No new namespace Pod Security label transition belongs to this release.

After upgrade, inspect native node history and retained container logs. Verify
an observation outage leaves runs unresolved, then catches up after reconnect.
The reported reason begins `status unknown: cluster API unreachable since`;
Argo's real phase decides completion after a live confirmation. Completed
Workflows found after downtime still need available-log capture and identity
retirement. Watch for missing-log reason lines rather than assuming every Pod
survives cleanup. Grants become database-only authority; no ConfigMap mirror
should be used to restore permissions.

The promotion FIFO queue is removed: promotions are planned immediately, and a
moved target follows the existing re-prepare/re-test path. The client plugin adds
`/oberth:runs` and `/oberth:watch` in v0.22.8. A failed CLI call displays its
sanitised first error line; a watch stops after three consecutive failures and
can be cleared with `/oberth:watch --off`. These remain DRAFT pending integration
and client qualification; no server behavior change is required by the plugin.

### DRAFT v0.23.0: source, credentials, PVCs and Pod Security

Before upgrade:

1. Stop new submissions and drain every pre-upgrade Workflow, including exit
   handlers. Since the v0.22.8 draft, version-changing upgrades wait by default,
   show a countdown and refuse at the bounded timeout; `--no-wait-idle` skips
   the wait. It remains advisory and does not freeze admission. Back up history
   and required external data.
2. Register the native source plugin through the installer. Review its image,
   public TLS trust, source listener on port 8445, NetworkPolicy and TokenReview
   permission. Validate the complete source authorization/lease/executor assembly;
   separate slice readiness is insufficient. With `--skip-argo-workflows`, the
   installer verifies the controller ConfigMap and never takes it over; takeover
   is limited to the Argo release installed by Oberth.
3. Review native Argo executor/controller env. Managed installation fills absent
   required settings with consent and refuses conflicts. External controllers
   must supply these native values themselves:

   ```yaml
   executor:
     env:
       - {name: KUBECONFIG, value: /var/run/secrets/kubernetes.io/serviceaccount/kubeconfig}
       - {name: ARGO_PROGRESS_PATCH_TICK_DURATION, value: '0'}
       - {name: ARGO_PROGRESS_FILE_TICK_DURATION, value: '0'}
   controller:
     extraEnv:
       - {name: ARGO_PROGRESS_PATCH_TICK_DURATION, value: '0'}
       - {name: ARGO_PROGRESS_FILE_TICK_DURATION, value: '0'}
   ```
4. Provision separate PVCs for each repository and trust tier, then grant their
   literal names with an unscoped administrator. For example:

   ```sh
   oberth access allow --kind pvc --tier ci upstream/org/repository build-cache-ci
   oberth access allow --kind pvc --tier release upstream/org/repository build-cache-release
   ```

   One claim stays bound to one repository/tier even after revocation. A CI step
   receiving secrets cannot mount a granted claim, including read-only. Use
   Pod-local scratch there. Kubernetes owns binding, access modes and placement.
5. Review namespace labels in the dry run: pipeline enforce=baseline, server
   enforce=restricted, preserving stricter operator policy. The reported installer
   applies them after successful rollout and restores old values on rollback
   before restarting the old chart. Old hostPath-bearing Workflows cannot safely
   continue creating Pods under the new floor.
6. Remove retired override keys `cache.ciRoot`, `cache.releaseRoot`,
   `argo.sourceStorageClass` and `retention.artifacts.limitBytes`. The reported
   upgrade retires stored keys automatically; age/budget retention for historical
   artifacts remains. Review utility-image retention for restore helpers.

After upgrade, deploy consuming repositories' source, credential, scratch and
cache YAML together. Exercise source-in, private CA login, retry/restart,
release preflight, separate CI/release claims and an assist hold under baseline.
Source files and history share 512 MiB, including sanitized Git metadata. Pins
retain their own limits. Busy parallel exports retry behind one active export.
Source is private per Pod; changing it in one Pod does not change a later Pod's
copy. No durable output collector remains. Historical files remain readable
with retention/purge, provided their bytes were preserved separately.

When no pre-upgrade Workflow or exit handler remains, review leftover PVCs and
seed Pods labelled `oberth.ci/component=source` in the pipeline namespace and
delete those old resources with explicit operator credentials. Workflow-owned
resources otherwise follow Argo TTL. New server code does not sweep unowned old
claims. Node cache directory disposal belongs to the node administrator.

Native template sharing is a further integration prerequisite: admission must
validate existing namespaced WorkflowTemplates, and the chart must protect
their identity fields against replacement after validation. No inspected slice
implements that admission yet.

### DRAFT v0.23.1: native schedules

Before upgrade, preserve schedule pause intent and back up history. Source-in
must activate native scheduling. Review CronWorkflow permissions, native
timezone/deadline/history settings and child adoption. The latest decision
requires one-time import of v0.22.x pauses as suspended CronWorkflows; reports
still identify pause import and activation as pending.

After upgrade, check one real fire/adoption, pause/resume/manual trigger,
revocation suspension, missed-run handling and removal with a running child.
Entry deletion must orphan running children. Reconciliation uses relevant
pushes, grant/identity/policy events and startup, not a new periodic loop.
Do not tag until activation is on and a schedule fired and was adopted on a
real Argo controller. Authored concurrency pass-through is required by the
latest decision but is not verified: inspected code still hard-codes Forbid.

## Repository authors

### v0.22.7: map run facts to environment explicitly

Before, the command could depend on injected run-fact env:

```yaml
container:
  image: BUILD_IMAGE
  command: [sh, -ec]
  args: ['printf "%s\n" "$OBERTH_SHA"']
```

After, map the native parameter explicitly:

```yaml
container:
  image: BUILD_IMAGE
  command: [sh, -ec]
  args: ['printf "%s\n" "$OBERTH_SHA"']
  env:
    - {name: OBERTH_SHA, value: '{{workflow.parameters.oberth-sha}}'}
    - {name: OBERTH_REPO, value: '{{workflow.parameters.oberth-repo}}'}
    - {name: OBERTH_REF, value: '{{workflow.parameters.oberth-ref}}'}
    - {name: OBERTH_TRIGGER, value: '{{workflow.parameters.oberth-trigger}}'}
    - {name: OBERTH_RUN_ID, value: '{{workflow.name}}'}
```

Release-only containers may also map `oberth-release-tag` and
`oberth-release-sha`; those parameters are absent on CI runs. The trigger is
`branch`, `promotion`, `schedule` or `tag`, independently of trust tier. Map
facts through env; do not interpolate ref/tag text directly into shell source
or use it in `when:`. Select by the closed trigger vocabulary instead.

Missing env mappings produce no dedicated admission refusal: the command sees
an unset variable. Do not declare the server parameter to fix that. Before:

```yaml
spec:
  arguments:
    parameters: [{name: oberth-sha, value: authored}]
```

After, remove that declaration and reference the parameter in env as above.
The exact refusal is:

```text
argoworkflow: spec.arguments.parameters["oberth-sha"] is server-owned; the oberth- parameter prefix is reserved; reference workflow parameters without declaring them
```

The same reservation applies to inputs, outputs, task/step/hook arguments and
output `globalName`. Refs outside the allowed alphabet get:

```text
ref name is invalid: start with a letter or digit; use only letters, digits and . _ / + @ = , -
```

Rename such refs upstream; do not expect existing unsupported names to be
silently removed. See [run facts](run-facts.md).

### v0.22.7: remove fragments; DRAFT v0.23.0: native sharing

Before removal, a fragment reference could use a repository/tag locator:

```yaml
steps:
  - - name: check
      templateRef:
        name: upstream/org/shared@v1.0.0
        template: check
```

For v0.22.7, copy the needed template into the consuming Workflow:

```yaml
steps:
  - - {name: check, template: check}
# Under spec.templates, alongside this steps template:
# - name: check
#   container: {image: BUILD_IMAGE, command: [check]}
```

For the first step of template `main`, the current exact refusal is:

```text
argoworkflow: spec.templates["main"].steps[0][0].templateRef points outside this document; inline the template
```

**DRAFT v0.23.0 target:** a shared pipeline repository installs a namespaced
WorkflowTemplate through its own capability-granted release step. Consuming
repositories reference the existing object:

```yaml
steps:
  - - name: check
      templateRef:
        name: shared-checks
        template: check
```

This is a target recipe, currently refused by the same message. Oberth must
admit and validate the existing template without fetching repository fragments
or rewriting it. Granted secret steps stay inline in consuming repositories;
`clusterScope: true` and `workflowTemplateRef` stay refused. The current latter
message is:

```text
argoworkflow: spec.workflowTemplateRef submits a spec Oberth cannot review; inline the templates
```

Future missing-template, shared-grant and cluster-scope messages have no verified
implementation. Exact new wording is pending; do not invent it or enable this
recipe until admission and the WorkflowTemplate identity policy land.

### v0.22.7: authored security and scratch

Before v0.22.7, the annotation selects nonroot leaves and the server supplies
security contexts and `/tmp`:

```yaml
metadata:
  annotations:
    oberth.ci/nonroot-templates: check
spec:
  templates:
    - name: check
      container: {image: BUILD_IMAGE, command: [check]}
```

After deploying v0.22.7, remove the annotation, author desired identity and
filesystem settings, and provide scratch explicitly. Example for a read-only main filesystem:

```yaml
spec:
  templates:
    - name: check
      volumes:
        - {name: scratch, emptyDir: {}}
      container:
        image: BUILD_IMAGE
        command: [check]
        securityContext:
          runAsUser: 1000
          runAsGroup: 1000
          readOnlyRootFilesystem: true
          allowPrivilegeEscalation: false
          capabilities: {drop: [ALL]}
          seccompProfile: {type: RuntimeDefault}
        volumeMounts:
          - {name: scratch, mountPath: /tmp}
```

Add compliant contexts to authored init containers, sidecars and containerSet
members too. Argo main/executor defaults do not cover every authored helper.
Root and writable root filesystems are permitted by the floor. Oberth preserves
an authored context; Argo supplies native defaults and Kubernetes enforces the
floor. Policy messages include exactly:

```text
Argo containers must set allowPrivilegeEscalation to false
Argo containers must drop ALL capabilities
Argo containers can add only NET_BIND_SERVICE
Argo step containers require effective RuntimeDefault or Localhost seccomp; other Pods cannot set Unconfined seccomp
```

Set those fields on the offending container or an effective native default.
Do not leave Pod-level Unconfined even when all containers override it.
Leaving the retired nonroot annotation in v0.22.7 gets:

```text
argoworkflow: metadata.annotations["oberth.ci/nonroot-templates"] is refused: keys under oberth.ci/ are assigned by Oberth and record which run owns the Workflow; remove the key or rename it outside oberth.ci/
```

Remove that annotation and use the authored security/scratch example above.

### v0.22.7: outputs on granted steps

These rules apply to secret-store and cloud-identity steps in v0.22.7, including
effective outputs inherited by name from `templateDefaults`. Even a relative
path on an unmounted working directory is refused. Override an inherited unsafe
output using the same output name and a safe absolute `valueFrom.path`; setting
only `value` or `default` leaves that inherited path in place. Before:

```yaml
outputs:
  parameters:
    - name: digest
      valueFrom: {path: result}
```

After, write an absolute file outside every mount and input-artifact path:

```yaml
outputs:
  parameters:
    - name: digest
      valueFrom: {path: /tmp/result}
```

This example needs a writable `/tmp` on the container filesystem, with no
volume mounted there. With a read-only filesystem and only mounted scratch,
there is no admissible output file path. For a single-container step, use
`outputs.result` or move output production to a step with a writable non-mounted
path. With sidecars or multiple containerSet members, remove file-read outputs and result references
or move the work to a step without those other containers.

Exact messages, with `publish`/`digest` as example names:

| Refusal | Fix |
| --- | --- |
| `argojob: credentialed template "publish" output "digest" read path "result" is relative; use an absolute path on the step's own filesystem` | Write an absolute non-mounted path. A path beginning with a substitution also gets this relative-path message. |
| `argojob: credentialed template "publish" output read path "/work/artifacts/result" is on a volume mount or cannot be resolved before execution` | Move the file outside mounts and remove path substitutions. |
| `argojob: credentialed template "publish" cannot use path-read output parameter "digest" because another container of the Pod can redirect what Argo reads` | Remove the read or move production to a step without sidecars/other members. |
| `argojob: credentialed template "publish" cannot use referenced outputs.result because another container of the Pod can redirect what Argo reads` | Remove the reference or use a step without other containers. |

Before/after for the last case, in the invoking parent:

```yaml
# Before: capture a credentialed step's stdout despite a sidecar.
arguments:
  parameters: [{name: digest, value: '{{steps.publish.outputs.result}}'}]
```

```yaml
# After: obtain the value from a separate suitable step.
arguments:
  parameters: [{name: digest, value: '{{steps.describe.outputs.result}}'}]
```

Argo's loose capture matcher also sees `steps.publish.outputs.resultSuffix`,
references in any field of the invoking parent (including labels), workflow
`spec.podMetadata`, applicable `templateDefaults`, and any substituted step/task
template name in a result-referencing parent even if it resolves elsewhere.
These get the referenced-result refusal above. Remove the capture-triggering
text, use literal invocations or move the output to a separate suitable step.
Sidecars remain allowed without these reads; init-only helpers do not alone
trigger this shared-container rule. ContainerSet members are covered by the
conditional rule, but credential delivery currently accepts Container/Script
leaves and does not enable credentialed containerSet templates. These guards are
not a boundary between containers of one step; Argo's trusted helpers retain native credential mirrors and the shared-template
startup race remains a documented residual.

### v0.22.7: new metadata refusal; native fields now admitted

Before, a Workflow could supply a controller-owned label:

```yaml
metadata:
  labels: {workflows.argoproj.io/controller-instanceid: authored}
```

After, remove it or use a literal key outside the reserved prefix:

```yaml
metadata:
  labels: {pipeline-purpose: check}
```

Exact message:

```text
argoworkflow: metadata.labels["workflows.argoproj.io/controller-instanceid"] is refused: keys under workflows.argoproj.io/ are assigned by the Argo controller and decide which controller runs the Workflow; remove the key or rename it outside workflows.argoproj.io/
```

The same applies to annotations and `spec.workflowMetadata`; the supported
`workflows.argoproj.io/pod-name-format` annotation is excepted. Native
`generateName`, camelCase names, recursion, `imagePullSecrets`, tag-only images,
resourceFieldRef, non-positive deadlines and larger native structures no longer
need workarounds for removed budgets. Retain resource requests appropriate for
Kubernetes. Memoization, host configuration, reserved identity/env and optional
operator image policy checks remain security refusals.

### Grants: already workflow-scoped in v0.22.5, retained in v0.22.7

No YAML stanza grants authority. Before, a declaration alone might be mistaken
for permission:

```yaml
metadata:
  annotations:
    oberth.ci/secret-paths: oberth/data/release/signing
```

After, retain that declaration for v0.22.7, use a stable inline named consumer,
and have an administrator approve its exact tuple:

```yaml
# Under spec.templates:
- name: publish
  container:
    image: CLIENT_IMAGE
    command: [/run/oberth/bin/oberth]
    args: [secretstore, exec, --path=oberth/data/release/signing, --, ./publish]
```

```json
{"repo":"upstream/org/repository","workflow":".oberth/release.yaml","step":"publish","secret":"oberth/data/release/signing"}
```

Pass the JSON to `access_allow`; revoke/dismiss require the workflow dimension
too. A wildcard covers steps within that exact file, never all workflow files.
Moving or renaming the file requires a grant for the new path. Exact unscoped
unscoped-approval message: `workflow is required; unscoped secret approvals are
rejected`. If a path is granted only to `build`, a different named consumer sees:

```text
secret grant denied: step "publish" not authorized for path "oberth/data/release/signing" -- granted to step "build" only
```

Approve the intended step/file/upstream tuple, then synchronize the store and
provision identities. Declaration alone is never a fix. DRAFT v0.23.0 removes
the annotation requirement and recognized-client dependency for grant selection;
the authored credential stanza below replaces injected delivery.

### v0.22.7: cancel and native verdict

Review exit handlers before relying on cancel. Before and after the upgrade,
your YAML may be identical:

```yaml
spec:
  onExit: finish
```

```yaml
spec:
  onExit: finish
```

There is no YAML refusal or migration for cancellation. The changed control is
ordinary branch cancel = Stop (exit handlers run), then a second active cancel =
Terminate (exit handlers skipped). Release/tag/apply and promotion cancellation
uses Terminate immediately. Restore terminates orphaned Workflows. Cleanup is
Argo-owned. A tolerated failed step stays failed in step evidence while Argo
Succeeded can pass and publish the Workflow. Explicit cancellation can suppress
publication even after native success; that passed run cannot be rerun. An older
green commit cannot overwrite a newer accepted branch head.

### DRAFT v0.22.8: author cleanup and use native node history

Before, omission relied on Oberth's per-Workflow TTL. After, omission uses the
operator's native Argo defaults, or declare a deliberate value:

```yaml
# Before: no cleanup field in spec.
spec:
  entrypoint: main
```

```yaml
# After: an explicit native cleanup policy, if needed.
spec:
  entrypoint: main
  ttlStrategy: {secondsAfterCompletion: 3600}
```

There is no new refusal: TTL and `podGC` pass through unchanged. Aggressive
cleanup can remove logs before capture or remove the Workflow before its verdict
is observed. Remove reliance on in-log step markers; native node IDs distinguish
retries, loops and duplicate names. There is no required marker-replacement YAML.

### DRAFT v0.23.0: declare source inputs and typed pins

Before, the server provided `/work/src`, and pins used a top-level extension:

```yaml
sources:
  - repository: upstream/org/producer
    commit: '0123456789abcdef0123456789abcdef01234567'
    path: /work/sources/producer
```

After, every Pod needing source declares it in that template:

```yaml
inputs:
  artifacts:
    - name: source
      path: /work/src
      mode: 0755
      plugin:
        name: oberth
        key: '{{workflow.parameters.oberth-source}}'
        configuration: '{"version":1,"kind":"main"}'
    - name: producer
      path: /work/sources/producer
      mode: 0755
      plugin:
        name: oberth
        key: '{{workflow.parameters.oberth-source}}'
        configuration: '{"version":1,"kind":"pin","repository":"upstream/org/producer","commit":"0123456789abcdef0123456789abcdef01234567"}'
```

Pins need an operator-granted consumer/producer pair. Use the canonical upstream
path, full lowercase commit and unique pin path; never a branch/tag alias or URL.
The key is a public locator reference, not a credential; do not declare its
server parameter. Main input path is exactly `/work/src`, mode 0755. Source
working copies are private per Pod; YAML must not assume a shared writable source.

An old `sources:` gets a strict decoder refusal whose cause includes
`unknown field "sources"`; its full composed message has not been executed in
this documentation task. Remove the extension and `oberth.ci/sources`, then
declare inputs. Available exact slice messages, using `check` at the first input:

| Refusal | Fix |
| --- | --- |
| `argoworkflow: spec.templates["check"].inputs.artifacts[0] requires plugin oberth and the exact {{workflow.parameters.oberth-source}} key` | Use the exact plugin/key above. |
| `argoworkflow: spec.templates["check"].inputs.artifacts[0] has an invalid source plugin configuration` | Use strict version-1 main/pin JSON without extra fields. |
| `argoworkflow: spec.templates["check"] main source path must be /work/src` | Use the fixed main destination. |
| `argoworkflow: spec.templates["check"] source input requires mode 0755 without recursive mode or archive/subpath overrides` | Set mode 0755 and remove the overrides. |
| `argoworkflow: granted template "publish" input "source" requires an exact oberth source or pin` | Replace raw/ambiguous forwarded inputs on granted steps with exact source descriptors. |

### DRAFT v0.23.0: newly refused source forwarding and paths

Workflow-level artifact arguments are newly refused. Before:

```yaml
spec:
  arguments:
    artifacts: [{name: source, from: '{{inputs.artifacts.source}}'}]
```

After, pass from-only artifacts through an explicit step/task invocation:

```yaml
steps:
  - - name: check
      template: check
      arguments:
        artifacts: [{name: source, from: '{{inputs.artifacts.source}}'}]
```

The enclosing composite needs its own input slot; the receiving Pod declares the
exact destination/mode. Granted leaves must resolve only to exact source/pins.
The new spec-argument refusal is quoted from slice code:

```text
argoworkflow: spec.arguments.artifacts is not admitted; pass artifacts through step or task arguments
```

Argument names must be literal. Before/after:

```yaml
artifacts: [{name: '{{inputs.parameters.slot}}', from: '{{inputs.artifacts.source}}'}]
```

```yaml
artifacts: [{name: source, from: '{{inputs.artifacts.source}}'}]
```

For a first step in `main`, the exact refusal is:

```text
argoworkflow: spec.templates["main"].steps[0][0].arguments.artifacts[0].name must be literal before source matching
```

The fix also applies to DAG and hook arguments. Input names must be literal and
unique too. Location-bearing argument artifacts get `must be a from-only
artifact argument`; put the descriptor on inputs, leaving arguments `{name, from}`.

For example, before/after for duplicate input names on template `check`:

```yaml
inputs:
  artifacts:
    - {name: config, path: /work/config-a, raw: {data: first}}
    - {name: config, path: /work/config-b, raw: {data: second}}
```

```yaml
inputs:
  artifacts:
    - {name: config-a, path: /work/config-a, raw: {data: first}}
    - {name: config-b, path: /work/config-b, raw: {data: second}}
```

Exact refusal: `argoworkflow: spec.templates["check"] has duplicate input artifact
name "config"`. Use unique names and update forwarding references. Templated
input names get `argoworkflow: spec.templates["check"].inputs.artifacts[0].name
must be literal before source matching`; use the literal names above.

Resource templates cannot take inputs, including raw/default-inherited inputs.
Before, a resource template might try to load its manifest from an artifact:

```yaml
inputs:
  artifacts: [{name: manifest, path: /work/manifest, raw: {data: authored}}]
resource:
  action: apply
  manifestFrom: {artifact: {name: manifest}}
```

```yaml
# After: keep the capability-granted resource template self-contained.
resource:
  action: apply
  manifest: |
    apiVersion: v1
    kind: ConfigMap
    metadata: {name: pipeline-setting}
    data: {setting: authored}
```

Exact source refusal: `argoworkflow: spec.templates["apply"] resource templates
cannot take input artifacts`. Remove inputs and use a reviewed manifest with
the required capability grant. This example does not itself confer that grant.

Paths normalizing to `/var` are newly refused, including `var`, `/var/x/..` and
`/tmp/../var`. Before/after for an ordinary raw input:

```yaml
inputs:
  artifacts: [{name: config, path: /var, raw: {data: settings}}]
```

```yaml
inputs:
  artifacts: [{name: config, path: /work/config, raw: {data: settings}}]
```

Exact refusal for template `check`:

```text
argoworkflow: spec.templates["check"].inputs.artifacts[0].path shadows a reserved credential/helper path
```

Granted output paths receive the same reserved-path check. Granted source inputs
cannot overlap mounts, including a mount above or below the input. Older
scaffolds mounting at `/work` need the following change before a source-consuming
step can hold a grant:

```yaml
volumeMounts: [{name: work, mountPath: /work}]
```

```yaml
volumeMounts: [{name: work, mountPath: /work/artifacts}]
```

Exact refusal and fix:

```text
argoworkflow: granted input path "/work/src" overlaps a volume mount "work" at "/work"; mount scratch beside the source path (for example /work/artifacts)
```

Do not collect a granted output parameter from that mounted scratch. Before:

```yaml
outputs:
  parameters: [{name: digest, valueFrom: {path: /work/artifacts/result}}]
```

```yaml
outputs:
  parameters: [{name: digest, valueFrom: {path: /tmp/result}}]
```

After requires a writable, unmounted `/tmp` and no conflicting credential path.
Exact message:

```text
argoworkflow: granted output parameter path "/work/artifacts/result" overlaps a volume mount "work" at "/work/artifacts"; write the output parameter file outside authored mounts and reserved credential paths
```

### DRAFT v0.23.0: author the secret-store stanza

Before, the server supplies the client, login token, CA, scratch and env for the
granted main:

```yaml
- name: publish
  container:
    image: RELEASE_IMAGE
    command: [/run/oberth/bin/oberth]
    args: [secretstore, exec, --dir=/run/private-files, --path=oberth/data/release/signing, --, ./publish]
```

After, keep the exact step/file/upstream grant and declare the materials yourself:

```yaml
- name: publish
  volumes:
    - name: login
      projected:
        sources:
          - serviceAccountToken:
              audience: oberth-secretstore
              expirationSeconds: 600
              path: token
    - name: store-ca
      configMap: {name: oberth-secretstore-ca}
    - name: private-files
      emptyDir: {medium: Memory, sizeLimit: 64Mi}
  container:
    image: IMAGE_WITH_CLIENT_AND_PUBLISH_COMMAND
    command: [oberth, secretstore, exec]
    args: [--dir=/run/private-files, --path=oberth/data/release/signing, --, ./publish]
    env:
      - {name: VAULT_ADDR, value: '{{workflow.parameters.oberth-secretstore-address}}'}
      - {name: VAULT_CACERT, value: /var/run/store-ca/ca.crt}
    volumeMounts:
      - {name: login, mountPath: /var/run/secrets/secretstore, readOnly: true}
      - {name: store-ca, mountPath: /var/run/store-ca, readOnly: true}
      - {name: private-files, mountPath: /run/private-files}
```

Use the exact installed `argo.vault.audience`; `oberth-secretstore` is the managed
in-cluster default. External upgrades retain their audience unless changed.
Coordinate audience changes through installer values and store synchronization
before matching YAML. Only `oberth-secretstore-address` is a store parameter;
there is no role parameter. The helper derives its role from the projected
identity. Other clients may read exactly `spec.serviceAccountName` through a
fieldRef. The helper image alone has no shell/publish tools; build/select a
suitable client image. Public CA is not a secret.

An omitted client has no dedicated admission refusal; execution fails when it
cannot find the old injected binary. Token/CA declarations must be on the named
granted local Container or Script leaf, with exactly one token projection,
expiration 600–3600, and one read-only main mount. Init containers and sidecars
cannot mount or mirror it. Ambient automount stays disabled. Argo's trusted
helpers retain native mirrors; scratch confidentiality is the author's duty.

Exact slice refusals for the named `login` volume (or `store-ca` for the CA row):

| Refusal suffix after `argoworkflow: spec.templates["publish"].volumes["login"] ` | Fix |
| --- | --- |
| `has no step grant for the exact token audience` | Approve the exact step/file/upstream and use the installed audience. |
| `requires exactly one serviceAccountToken projection` | Remove extra projection sources. |
| `requires expirationSeconds between 600 and 3600` | Set a value in that range. |
| `requires the secret-store login token at /var/run/secrets/secretstore/token` | Use the exact mount directory and token path above. |
| `must be mounted read-only at a literal path in main` | Use a literal read-only mount without token subPath. |
| `cannot be mounted in an init container or sidecar` | Remove that authored helper mount; keep the main mount. |
| `may name only the install-created public CA ConfigMap` | Use the installed public CA name, not another ConfigMap. |

For example, fix a wrong audience by replacing
`serviceAccountToken: {audience: wrong, expirationSeconds: 600, path: token}`
with the stanza above. Use separate non-credential helper volumes when needed.

### DRAFT v0.23.0: replace caches and injected tool settings

Before, commands could rely on injected cache/workspace env and host directories:

```yaml
container:
  image: BUILD_IMAGE
  command: [sh, -ec]
  args: ['GOCACHE="$OBERTH_CACHE_DIR/gobuild" go build ./...']
```

After, reference an existing administrator-granted claim and author all env:

```yaml
spec:
  volumes:
    - name: build-cache
      persistentVolumeClaim: {claimName: build-cache-ci}
  templates:
    - name: build
      container:
        image: BUILD_IMAGE
        command: [go, build, ./...]
        env:
          - {name: GOCACHE, value: /cache/gobuild}
          - {name: GOMODCACHE, value: /cache/gomod}
        volumeMounts:
          - {name: build-cache, mountPath: /cache}
```

No cache, mount, subpath, tool env or workspace annotation is supplied for you.
Missing env alone has no admission refusal. For example, remove these old
template metadata annotations before using the explicit mounts/env above:

```yaml
# Before, on the build template:
metadata:
  annotations:
    oberth.ci/workspace-mounts: cache
    oberth.ci/workspace-env: none
```

```yaml
# After, on the build template:
metadata: {}
```

Remove retired workspace annotations;
their inspected template-metadata refusal is `argoworkflow:
spec.templates["build"].metadata.annotations is assigned by Oberth; Pod annotations
select security profiles and injection webhooks`. Declare the mounts/env directly.

Exact missing-grant message for a claim volume in spec:

```text
argoworkflow: spec.volumes["build-cache"] references an existing PersistentVolumeClaim "build-cache-ci"; requires an administrator grant for this repository and tier
```

Grant the existing claim for this repository and tier, or remove the reference.
Offline `oberth validate` has no repository grants and cannot prove that access.
A CI secret step gets, for a writable mount:

```text
argojob: CI secret step "publish" cannot mount granted PVC "build-cache" (read-only: false)
```

Changing it to read-only is still refused. Instead replace the claim with local
scratch on that step:

```yaml
volumes:
  - {name: private-build, emptyDir: {}}
container:
  image: BUILD_IMAGE
  volumeMounts: [{name: private-build, mountPath: /cache}]
```

Authored hostPath gets `argoworkflow: spec.volumes["build-cache"] mounts the
node filesystem and is not admitted`. Replace it with a granted PVC as above.
The new cluster policy also refuses former server-injected hostPath caches.

### DRAFT v0.23.0: scratch replaces collected artifacts

Before, the artifact mount/env could be implicit:

```yaml
container:
  image: BUILD_IMAGE
  command: [sh, -ec]
  args: ['mkdir -p "$OBERTH_ARTIFACTS"; cp report.txt "$OBERTH_ARTIFACTS/"']
```

After, declare Pod-local scratch explicitly and publish needed files through
your own release steps:

```yaml
volumes:
  - {name: scratch, emptyDir: {}}
container:
  image: BUILD_IMAGE
  command: [sh, -ec]
  args: ['cp report.txt /work/artifacts/']
  volumeMounts: [{name: scratch, mountPath: /work/artifacts}]
```

That scratch is uncollected and is not a handoff to another Pod. Missing
`OBERTH_ARTIFACTS` has no special refusal. Do not replace it with native output
artifacts: there is no output backend. Before/after for refused output settings:

```yaml
spec:
  archiveLogs: true
  artifactGC: {strategy: OnWorkflowCompletion}
  templates:
    - name: check
      outputs:
        artifacts: [{name: report, path: /work/artifacts/report.txt}]
```

```yaml
spec:
  archiveLogs: false
  templates:
    - name: check
      outputs:
        parameters: [{name: count, value: '1'}]
```

Remove artifactGC, output artifacts and archive settings. Exact messages:

```text
argoworkflow: spec.archiveLogs writes output artifacts and is not admitted
argoworkflow: spec.artifactGC is not admitted; source delivery is input only
argoworkflow: spec.templates["check"].outputs.artifacts is not admitted; source delivery is input only
```

Ordinary parameters/results remain native, subject to credential-output guards.
Historical stored artifacts remain readable until retention or purge; no new
collector or output-slot mechanism replaces them.

### DRAFT v0.23.1: schedules become CronWorkflows

Keep the existing repository declaration, without adding a timezone key before
the cutover:

```yaml
# Before: .oberth/schedule.yaml
schedules:
  - {name: nightly, cron: '0 3 * * *', branch: main}
```

```yaml
# After: .oberth/schedule.yaml
schedules:
  - {name: nightly, cron: '0 3 * * *', branch: main, timezone: UTC}
```

The DRAFT after-example adds optional `timezone: UTC`; the old strict parser
accepts only `name`, `cron` and `branch`. Omit timezone after the cutover too if
you want an unchanged declaration. Keep the generated native timezone separate.

The target generated native object, shown only to explain execution semantics:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: CronWorkflow
spec:
  schedules: ['0 3 * * *']
  timezone: UTC
  # concurrencyPolicy must follow authored intent; Argo defaults when absent.
  # workflowSpec contains the admitted pipeline.
```

Do not submit that CronWorkflow as the repository pipeline or add speculative
keys to `schedule.yaml`. A submitted CronWorkflow currently gets:

```text
argoworkflow: kind must be "Workflow", got "CronWorkflow"
```

Keep the schedule file and let synchronization own the native object. The latest
owner decision requires concurrency pass-through/default Allow; inspected code
still hard-codes Forbid and does not establish the repository field for this.
Exact new concurrency/activation refusals are therefore unverified. Minimum
interval and entry-cap checks remain. Native deadline/history and missed-fire
semantics must be reviewed before activation; do not carry forward assumptions
about the deleted local cursor and coalescing engine.
