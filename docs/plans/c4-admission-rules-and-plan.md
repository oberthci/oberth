# C4: Admission rules and plan

## Purpose

`pkg/argoworkflow` is on `main`. C4 validates repository-authored native Argo YAML
against Oberth's admission, identity,
grant and source boundaries. It supplies the declarations and logical step plan for one
prepared admission. YAML defines the work; Kubernetes places Pods and Argo executes it.
C4 does not execute repository code, schedule runs, evaluate dependency expressions,
predict resource peaks, provision identities, authorize source downloads, persist
history, collect logs or publish events.

## Owned data and public contract

The [Shared contracts](README.md#shared-contracts) are normative for shared admission,
plan, step/attempt/count, failure/retry, actor, source, Git, event, identity and
runtime-configuration types. Local descriptions below specify component behavior and use
those definitions; they do not define rival DTOs.

- **Documents and policy.** `Decode` accepts exactly one strict `argoproj.io/v1alpha1
  Workflow`, rejecting unknown fields, duplicate YAML keys, wrong kinds and extra
  documents. `Policy` holds administrator-supplied image prefixes, PVC claims, resource
  capabilities, exact token audiences and a namespace-bound `TemplateGetter`. Empty
  image prefixes admit any image; absent grant authority grants nothing. `Admit` checks
  all authored templates, including unused ones and inline carriers. Reachability
  narrows the display plan, never security inspection.
- **Store grants.** Admission selects verified store grants once by the structural
  template key, never a display name; inline templates select wildcard grants only.
  One step uses one store account. If any verified exact step grant applies,
  every declared secret path needs an exact grant; wildcard grants cannot fill gaps. Otherwise
  wildcard grants apply. Refusal prints the exact `access allow` command for each
  missing path. The sealed result carries this selection; C5 consumes it without
  selecting again.
- **Identity.** `Identity` records the pipeline and optional resource accounts. The pipeline
  account is the executor account. Preparation replaces authored account names
  with the assigned pipeline or granted step account.
  `Prepare` admits before `ForceIdentity`; the latter clears authored status, assigns
  namespace, normalizes every identity-bearing field and disables ambient token
  automount. Explicit granted projections remain separate from ambient automount. C3/C5
  select accounts and verify live account existence and UIDs; C4 creates none.
- **Delta:** C4 alone constructs `PreparedAdmission` through `Prepare`, called by C3.
  Its fields, digest, C5 hooks and copy-on-read semantics are pinned in [Shared
  contracts](README.md#shared-contracts). Audit, Create, source intent and plan
  explanation consume that same result. Admission checks grants and account identity
  once before sealing. Adoption verifies ownership of the same recorded Workflow.
- **Binding hooks.** C4 calls PrepareHooks.PrepareSources with all discovered descriptors,
  then BindSource for each declaration; C3 decides distinct allowed pins in parallel
  once per admission and supplies exact producer authority and revision binding.
  C4 records template/artifact/path coordinates; C3 does not parse YAML to prefill bindings.
- **Source declarations.** `SourceInputInventory` records effective inputs, forwarding,
  pinned destinations and shared declarations; it proves no access. Main input uses
  plugin `oberth`, key `{{workflow.parameters.oberth-source}}`, configuration
  `{"version":1,"kind":"main"}`, destination `/work/src`, mode `0755`. Pin configuration
  adds `kind:"pin"`, canonical `repository` and full lowercase `commit`, at a unique
  `/work/sources/name`. C6 owns descriptor wire semantics, recorded source bindings and
  source access; the locator is nonsecret and grants nothing. Use the sourcecontract
  values without importing source delivery.
- **Run facts.** `RunTrigger` is `branch|promotion|schedule|tag`; a tag event does not
  itself grant release authority. C3 selects build/release or an approved routed file.
  C4 accepts that selection and checks/stamps reserved parameters, including
  `oberth-trigger`, `oberth-schedule`, `oberth-source` and `oberth-secretstore-address`.
  Verified releases also receive `version`, `release-tested-commit`,
  `release-default-commit` and `release-build-status` from C3's server-only hook.
  In release scope, authored workflow arguments/defaults and global outputs cannot replace
  these facts; dynamic global output names are refused. Ordinary CI keeps authored parameters.
  Release-file inspection uses schema-only placeholders; build checks cannot invent these values.
  Authored declarations under `oberth-` remain refused; references are allowed. A named
  schedule carries its exact durable entry, never a guessed entry. require the exact
  trigger; incomplete run bindings cannot gain authority.
- One plan comes from admitted YAML plus the retained template/default snapshots. Walk
  entrypoint, DAG/steps, inline calls, onExit and lifecycle hooks, including named shared
  references. Include admitted container, script, containerSet, resource and suspend leaves,
  including a leaf entrypoint. Exclude structural grouping and retry nodes. Identity is the
  native invocation path without Workflow prefix or retry suffix; display names cannot establish
  identity. Display order is static run order: DAG tasks by dependency level (ties in
  declaration order), steps groups in order, a called template in place; never observed runtime
  order. Leaf siblings at the same local DAG dependency level or steps group carry a positive `parallel_group`; serial leaves and hooks carry none. A workflow or template with `parallelism: 1` has no groups. Mutually exclusive `when` branches remain grouped by their static dependency level; grouping does not evaluate runtime conditions. Literal `withItems` expands statically; dynamic leaf loops carry `fan_out` and their observed children inherit that task's group, while compound or unresolved template descendants do not. A lone leaf loop gets its own group. Runtime-dependent fan-out and recursion retain
  an expandable entry and make exactness false. Native observations expand that entry in the
  display projection and fold retry attempts into it. They never rewrite the immutable admitted
  plan. A leaf not reached remains pending in the display, without a new run status. Public
  totals count the complete logical display projection, including observed dynamic expansion,
  independently of paging; `steps_planned_exact` is false while expansion is unresolved.
  The admitted plan remains immutable. Every run admitted for execution has a
  plan; a failed receive record without admission has none. Credential flags
  describe intent, never proof of a secret read or permission for replay.
- **Surfaces and findings.** C4 exports Go contracts, not HTTP/MCP handlers or commands.
  C11's `oberth validate` calls Decode, Admit, C4-owned native schema validation and the
  shared plan. the plan check and C1/C2 run views share the prepared result and the one
  step model. Explanation includes selected file/route, identities, required
  grants/audiences, source/cache destinations, authored resources and deadlines, helper
  configuration and enabled optional hardening. Mark offline, snapshot-checked,
  live-required and unknown facts explicitly; unknown is no pass. Public records use
  snake_case. Use the Go names and JSON tags in [Shared
  contracts](README.md#shared-contracts), including nonserialized `Template`.
  `AdmissionError` preserves deterministic refusal causes; `TemplateReadError`
  distinguishes a failed live read. C1/C2 render actionable findings and events; C4
  supplies no event stream or audit writer. Collect all independently diagnosable
  offenders with their paths and remedies. Each finding carries its own optional
  `Command` from the existing grant command builder; C3 prints that fix beneath
  that finding and persists it for rejected-run replay. Commands stay separate
  from bounded remedies and are not duplicated in unstructured detail.

## Allowed dependencies

C4 uses the exact C4 allowlist in [dependencies.json](dependencies.json). Only pure
shared admission/step/source/identity values, runtime snapshots, defaults/specsize
helpers and native schema/codec libraries are allowed. C3 alone calls `Prepare`; C5 and
C10 call only C4 native-schema validation. C11 validation calls the schema/plan readers,
never Prepare directly. C6 imports only defaults/specsize helpers. C9/C13/C14 consume
shared constants or public CLI checks. C4 imports no C1–C3 or C5–C14 runtime
implementation, SQL, cluster client, source transport or installer. Template reads use
bounded injected getters.

## Invariants and budgets

Record each binding's checkout inventory once before sealing: tested commit, own ref,
default-branch head and bounded tag objects including noncommit targets. Other branch
heads are not delivered. The prepared admission is the sole source description.

At push, refuse any task reference to `outputs.result` from a template that takes the
Oberth source input. Reason: "result of a source step is not readable"; remedy: "write
the value to a file and use an output parameter".

**Delta:** C4 owns the display-plan cap: 2048 entries and 1 MiB of encoded Plan JSON.
When a literal loop expansion would exceed either bound, collapse that loop to one
dynamic placeholder at its invocation path, with `Plan.Exact=false`. If necessary
collapse the enclosing invocation up to a root placeholder, retaining declaration order
and conservative credential intent. Planner visit exhaustion uses the same placeholder
rule. Admission never refuses for plan expansion; this is no native fan-out or Pod
limit. C7 stores the result; C5 maps native boundary prefixes linearly and C2 expands
the display projection. At terminal completion resolve a dynamic remainder to unknown,
never leave it pending. One `containerSet` invocation is one logical step; its
containers keep separate log selectors within native attempt evidence.

- Refuse identity patches, host access, arbitrary Secret reads, reserved ownership
  metadata, executor names/volumes/env and ungranted credentials in every carrier,
  including defaults, inline calls and hooks. Preserve native placement, resources,
  priority, retries and optional deadlines. Mutex names must be literal
  `<upstream>/<organization>/<repository>/<lock>` for the run's repository; mutex and semaphore namespaces must be omitted. Offline validation without a repository checks only the mutex shape, with nonempty literal parts.
  Semaphore reads stay in same-namespace `oberth-sync-limits`. No CPU math, capacity queue, numeric resource grant,
  release lock or C4 run cap; the sole run cap belongs to Argo globally.
- Validate exact grant audiences and main-only read-only token mounts. Projections
  expire in 600–3600 seconds; the store login token is mounted under the reserved
  `/run/oberth` root.
  Check writes to credential delivery paths, including inherited fields. Resource
  templates require exact namespace/kind/action capability and a distinct resource
  **Delta:** executor; no secret or release-identity combination. Only namespaced
  kinds are grantable; Namespace, ClusterRoleBinding and other cluster-scoped
  kinds refuse regardless of grant. A grant authorizes only its own kind of
  access: a secret-store path never authorizes a Kubernetes Secret of the same name. No grant permits a
  resource template to create Workflow, CronWorkflow, WorkflowTemplate or
  ClusterWorkflowTemplate, or to act in Oberth's own namespaces. Image pull
  secrets require the same exact Secret grants as other Secret uses.
**Delta:** - Authored volumes are an allowlist: emptyDir, downwardAPI, projected without
  tokens, granted Secret, ConfigMap and granted PVC. Projected sources allow only
  Secret, ConfigMap and downwardAPI, with Secret grants checked. Explicit granted token
  projections are inserted by preparation, not arbitrary authored projections.
  HTTP/Git/S3 input artifacts, standard labels and downwardAPI volumes pass
  ordinary native validation. Keep Argo's own schema validator as the native
  validator; no substitute schema or second runtime policy layer.
- Named namespaced templateRefs are validated without inlining the submitted Workflow or
  fetching repository fragments. Shared templates inherit no consumer secret/PVC grant.
  A Workflow may combine image pull secrets with shared templates: check each
  consuming step's pull secrets against the calling repository's grants.
  Cluster-scope and whole-workflow refs stay refused. Offline deferral is explicit and
  cannot authorize Create. Grants are checked during preparation; the sealed result
  records the decision.
- Require main source for reachable container/script leaves whose effective workingDir
  is `/work/src` or below, unless an authored volume covers that directory. Include
  defaults and mixed invocation origins; report every offender. Do not search commands
  or env for source intent. **Delta:** C4 inventories the explicit author declaration
  and C5’s automatic source-trust hook, defined only in [C5 invariant
  4](c5-native-execution.md#invariants-and-budgets). C4 adds no checkout, cache, tool
  environment, secret client or working directory.
- Admission does not refuse hostAliases, DNS settings, downward fields,
  schedulerName, PDB, resource claims, sysctls or priorityClassName.
  [C9 invariant 7](c9-install-and-upgrade.md#invariants) owns the default floor
  and egress; a pipeline namespace C9 creates carries the baseline label,
  which refuses non-baseline sysctls. Core identity/credential protection
  remains mandatory.
- Keep boundary work bounded: document bytes ≤1 MiB; source-consuming submitted spec
  JSON ≤512 KiB, excluding metadata/status; nesting depth 64; planner work 65,536
  visits; at most four pinned destinations, 32 declared secret paths, 32 capability
  kinds, and schedule entry ≤64 bytes. These are boundary limits, not fan-out,
  Pod-count, storage-size or execution-time budgets. Exhaustion or incomplete safety
  evidence must never become a complete plan or proof of safe replay.

Authored workflow-level podSpecPatch refuses with a reason explaining that
Oberth owns the executor token projection. Template patches cannot touch that
volume or executor containers.

Admission refuses host paths, host namespaces, privileged containers and the server’s
accounts or volumes in every carrier, including defaults. A container resource other than
cpu, memory, ephemeral-storage and hugepages, such as a device plugin's device, needs an
exact device grant for its repository, file and step. Additional sysctl,
downward-field, archive and output-exposure restrictions are optional and off by
default. Authored field names are case-sensitive; another spelling cannot widen a grant
or replace a recorded source input.

Admission owns and exports the reserved mount roots, including the native runtime directory; authored mounts at, under or above a reserved root and mount paths with a `..` component are refused.

## Inspections

The requirement sentence is the inspection contract. Contract tests run on `main`;
walk inspections require real source delivery.

| Requirement | Inspection |
| --- | --- |
| [REQ-C4-001](REQUIREMENTS.md#req-c4-001) | `TestExactGrantedSourceAndSecretPathsContract` (contract_test) |
| [REQ-C4-003](REQUIREMENTS.md#req-c4-003) | `TestExactNativeCapabilitiesContract` (contract_test) |
| [REQ-C4-004](REQUIREMENTS.md#req-c4-004) | `TestFiniteCanonicalPreparationContract` (contract_test) |
| [REQ-C4-007](REQUIREMENTS.md#req-c4-007) | `TestHelpfulPreparationFailuresContract` (contract_test) |
| [REQ-C4-011](REQUIREMENTS.md#req-c4-011) | `push` (walk_step) |
| [REQ-C4-012](REQUIREMENTS.md#req-c4-012) | `TestGrantedMountPathsContract` (contract_test) |
| [REQ-C4-013](REQUIREMENTS.md#req-c4-013) | `TestServerStateBoundaryContract` (contract_test) |
| [REQ-C4-014](REQUIREMENTS.md#req-c4-014) | `grant` (walk_step) |

**Delta:** The `push` walk refuses all independent offenders before starting a run. Its
source-result negative control expects reason "result of a source step is not readable"
and remedy "write the value to a file and use an output parameter"; the file-output
alternative runs with real source delivery. `ungranted-secret` prints the exact operator
command, `grant` runs it unchanged, and a repeated `push` succeeds.
`TestExactNativeCapabilitiesContract` refuses each native workload kind and
Oberth namespace even with a matching grant. `TestExactGrantedSourceAndSecretPathsContract`
checks image pull Secrets; `TestGrantedMountPathsContract` exercises every allowed
volume and an unlisted type, authored tokens and protected executor patches.
Ordinary artifacts, labels and downwardAPI volume cases remain positive controls.
`TestPreparedPlanCoversActualControllerLeaves` permanently compares the prepared
plan with the real Argo controller.
The grant tests refuse store-path/Kubernetes-Secret substitution and unlisted
projected sources, and admit granted pull secrets with shared templates.
`TestPreparationPlanContract` covers roots, hooks, loops, retries, referenced templates,
case-sensitive fields and collapsed dynamic placeholders. Each boundary test starts from
admissible YAML and changes one forbidden feature.

Open follow-ups:

- Print exact grant commands and matching remedies for identity audiences and PVCs.
- Give a resource manifest missing its namespace a specific remedy.
- Decide whether to refuse the whole argoproj.io resource group.

## Retired terms

Pipeline DSL and fragments → native Workflow YAML and namespaced templates; source
injection → authored input artifacts; phase groups → logical steps.
