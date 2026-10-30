# Repository-authored KubeVirt VMIs

An administrator grants a registered repository the KubeVirt capability with
`kubevirt_grant` (MCP) or `oberth kubevirt grant <repo>` in the server pod.
`kubevirt_revoke` and `oberth kubevirt revoke <repo>` remove it. Each change
is audited. The grant alone does not permit arbitrary Kubernetes resources:
Oberth admits only bounded VMI manifests in named Argo `resource:` templates.

The same grant also authorizes KVM device leaves: container or script leaves
named in `oberth.ci/kvm-templates`, including the guest-test lanes of
`oberth.ci/guest-test-templates` (see `docs/guest-test-lanes.md`). Without an
active grant such a workflow fails closed at admission. A KVM leaf declares no
VMI demand, so the VMI budget below neither counts nor throttles it; it is an
ordinary weighted pod that receives `devices.kubevirt.io/kvm: 1`. Repositories
that already declared `oberth.ci/kvm-templates` before KVM leaves required the
grant need one before their next run. Check such a document locally with
`oberth validate --kubevirt`; without the flag, KVM leaves are refused.

## Repository VMI budget

Each grant carries a budget across its admitted runs: concurrent VMI count,
`spec.domain.cpu.cores`, and guest memory requests. A grant made
without explicit values gets 2 VMs, 6 cores and 12Gi, which is also the budget
of every grant made before schema v25. To raise it:

```
oberth kubevirt grant <repo> --max-vms 3 --max-cores 12 --max-memory 24Gi
```

or MCP `kubevirt_grant` with `max_vms`, `max_cores` and `max_memory` (a
Kubernetes quantity). Unset fields take the defaults, not the previous values.
Granting an active grant again with a different budget replaces the budget;
the audit entry records both the new and the previous budget.
`kubevirt_grant_get` and `oberth kubevirt get` show the budget.
Bounds: 1..16 VMs, 1..64 cores, 1Gi..256Gi.

The server applies a cluster-wide ceiling to every grant at admission:
`argo.kubevirtCeiling.{maxVMs,maxCores,maxMemory}` (server flags
`--argo-kubevirt-max-vms`, `--argo-kubevirt-max-cores`,
`--argo-kubevirt-max-memory`), by default 8 VMs, 24 cores and 48Gi. The
effective budget is the smaller of the grant and the ceiling, field by field.
A run may declare at most three VMI resource templates per budgeted VM; when
proven phase separation permits more declared creates, the limit is three per
declared create. The overall workflow/template limits still apply. A VMI's CPU request may not
exceed its cores, so the core budget also bounds what the VMIs reserve. The
budget counts guest memory; each virt-launcher Pod adds KubeVirt's own
overhead (a few hundred MiB) on top. The scheduler reserves each run's declared
VM count, cores and memory together with its ordinary admission weight. Two or
more runs proceed concurrently when their total fits the repository grant.
A run that would exceed the remaining capacity stays queued with reason
`KubeVirt capacity`, keeps its FIFO place, and holds no weight or concurrency
group until capacity frees. A single run exceeding the grant still fails
admission. Completion, cancellation and supersession release the reservation
once execution is terminal or Workflow deletion is accepted. On restart, adopted
branch Workflows retain their VM demand from the verified live spec. Legacy
VMI Workflows without the server quota binding are refused immediately during
adoption with a drain-before-upgrade hint.
Each is counted exactly once before new work is admitted, even if its grant
was reduced or revoked while it ran; new work waits for capacity to drain.

VMI resources do not select a run type or an implicit `kubevirt` group. An
ungrouped VMI workflow has no group badge. Any valid
`oberth.ci/concurrency-group` remains available to serialize work explicitly,
including across branch, promotion and tag runs. Different repositories have
independent grants, so size grants and the ceiling with their combined demand
in mind. Check a workflow locally against a
budget with `oberth validate --kubevirt --container-disk-prefixes=...
--kubevirt-max-vms 3 --kubevirt-max-cores 12 --kubevirt-max-memory 24Gi`.

Set `argo.containerDiskPrefixes` to clean repository-path prefixes such as
`quay.io/example/`. The corresponding server flag is
`--argo-container-disk-prefixes`. Images must be digest-pinned and match a
prefix. An empty list refuses VMI creation even for a granted repository.
For local admission checks, run `oberth validate --kubevirt
--container-disk-prefixes=quay.io/example/`; this checks the manifest but does
not grant the repository capability.
The chart creates `argo.vmLauncherServiceAccount` in the CI namespace and
gives it a VMI get/list/watch Role. The server performs creates and deletes. The shared Argo executor account receives no VMI
permission. Resource templates use the launcher as their per-template
executor; other steps keep the ordinary, tokenless pipeline identity.
For a resource Pod, Oberth resolves Argo's node ID to the named VMI template,
creates a Workflow-owned identity Pod under the launcher ServiceAccount, and
delivers a short-lived Pod-bound launcher token to Argo's executor containers.
Other Pod types receive the shared executor token. The resource Pod's `main`
container may mount that token only when it runs the pinned Argo executor image.

The VMI manifest must use `kubevirt.io/v1`,
`kind: VirtualMachineInstance`, `metadata.name:
"{{workflow.name}}-<suffix>"`, and exactly one label,
`oberth.ci/workflow: "{{workflow.name}}"`. `setOwnerReference: true` is
required. Delete manifests may also carry `metadata.uid`, either a literal UID
or exactly `"{{inputs.parameters.uid}}"` (using a named template input). Other
substitutions remain refused. The manifest must be
one YAML document with no carriage returns. Create manifests are limited to
the admitted containerDisk, inline cloud-init, emptyDisk, pod networking,
masquerade interface, CPU, memory, architecture and kernelBoot fields. Get
and delete manifests carry metadata only. Each VMI is limited to 4 cores and
8Gi; the repository budget above bounds concurrency across admitted runs.
Admission and the server create guard both enforce the budget. Credentialed workflows cannot contain VMI resource templates.

## Proven VM phases and the create guard

A resource create task finishes while its VMI is still alive. A dependency on
that create alone therefore earns no concurrency discount. The lifetime
projection counts each create invocation separately, including repeated use of
one template or VM name. Its count, cores and memory are each capped at the
corresponding per-template sum: a create template has one fixed VMI name, so
repeated invocations cannot have two live VMIs under that name. Two invocations
are non-overlapping only if a matching explicit delete is a descendant of the
earlier create **and an ancestor of the later create**.
Exit-handler cleanup never proves a barrier. Deletes use Argo's normal waiting
semantics; authored resource flags and success/failure conditions remain refused.

For bounded DAGs and steps, Oberth computes the maximum concurrent count, cores
and memory independently from these lifetime relations. For example, two
parallel 2-core/6Gi VMs followed by serial create/use/delete phases reserve
2 VMs, 4 cores and 12Gi, even when four different create templates exist.
The context-free sizing API remains conservative. During a real run, sizing
also uses the durable run trigger and the exact policy-admitted fragment
resolution that final submission uses. The demand receipt binds the run,
repository, executed commit, pipeline file, trigger, schedule entry, source
digest and resolved graph digest. A changed source or resolution cannot use an
earlier reservation. Recovery restores the peak recorded with the submitted
Workflow; it does not resolve fragment tags again.

The run-aware projection collapses a resolved subgraph only after proving it
contains no VMI creates or deletes. A get-only address template may therefore
have a bounded retry limit (0–10) without disabling the proof. Retries around
creates or deletes, unresolved references, dynamic fan-out, cycles, lifecycle
hooks, unsupported dependency expressions and exit-handler creates keep the
conservative sum. A dependency mixing top-level `&&` and `||` must put each
OR group in parentheses; otherwise the projection falls back rather than
assuming a delete is required under Argo's operator precedence. Every DAG task
name must be usable as an Argo expression identifier. The server applies a
[vendored, parity-tested copy of Argo's bare/legacy expansion](../third_party/argo-depends/README.md)
and hyphen normalization, then compiles each dependency
against boolean task-result fields. A compile failure (including keyword names
such as `in`, `not`, `true` or `if`) keeps the sum: Argo can mark a task Errored
before its dependency gate succeeds. Compilation executes no expression or
planner, and its result is reused for each invocation of the resolved template.
Work is bounded to 4,096 shared classification/expansion and scenario-proof
checks, 256 projected nodes and at most 32 partition literals. A scenario with more than
16 enabled create invocations also keeps the sum. Demand is computed once per
immutable resolved source; repeated admission still checks the current policy.
Successful completion of a composite template alone does not prove every child
delete ran successfully.

The supported `when` grammar consists of an absent/empty guard (`when: ""`),
bare `true` or `false`, a trigger comparison, or a safe partition equality as
described below. A whitespace-only guard is an error in Argo; it is unsupported
for projection and retains the conservative sum, rather than counting as true.
These boolean operands may be combined with `&&`, `||` and parentheses. A trigger
comparison has `{{workflow.parameters.oberth-trigger}}` on the left and a literal
`branch`, `promotion`, `schedule` or `tag` on the right, using `==` or `!=`;
either side may be quoted. Every operand in the same authored guard must parse
before any folding: `false && X` and `true || X` remain unsupported when `X` is
unparsed. Ternary/coalesce operators, raw output interpolation, arbitrary
indexing/calls, backslashes, commas and missing operands earn no proof. Brackets
are supported only inside the exact safe partition expression tags below.

YAML defaults, environment values and claimed trigger parameters never select
the run trigger. The server retains **every authored `when` string byte-for-byte**.

Schedule partitions earn mutual-exclusion credit only for guards already
written as an entire safe expression tag with a quoted string literal:

```yaml
inputs:
  parameters:
    - name: partition
dag:
  tasks:
    - name: partition-0
      template: vm-phases
      when: "{{=inputs.parameters['partition'] == '0'}}"
    - name: partition-1
      template: vm-phases
      when: "{{=inputs.parameters['partition'] == '1'}}"
```

The input must be explicitly supplied by that invocation, either as a literal
or as the named final scalar output of a resolved, lifetime-neutral predecessor.
For example, the caller of the partitioned template can pass its producer's
output as ordinary parameter data:

```yaml
- name: schedule-parts
  template: partitioned-phases
  depends: select.Succeeded
  arguments:
    parameters:
      - name: partition
        value: "{{tasks.select.outputs.parameters.partition}}"
```

An input supplied only through a YAML default is not a binding. Output-based
bindings additionally require an explicit `producer.Succeeded` prerequisite on
the consuming DAG task. Mere ancestry, bare dependencies and terminal-state ORs
are insufficient: Argo can substitute an absent-output sentinel for a skipped
or omitted producer and then use each consumer's own input fallback. In steps
workflows, only a provably unconditional producer in an earlier step group
supplies a shared output scalar.

A consumer input with `default`, `value` or `valueFrom` cannot earn shared-scalar
credit from an unknown output, even when the producer must succeed: a producer
can emit Argo's internal absent-output sentinel literally and trigger those
fallbacks. Such graphs retain summed demand. Explicit closed literal arguments
remain supported; defaults alone never establish exclusivity.
Oberth never runs a planner before reservation. Direct predecessor-output
guards may use `{{=tasks['select'].outputs.parameters['partition'] == '0'}}`
(or `steps['select']` in a steps workflow). Bracket keys and string literals
use single quotes; names use ASCII letters/digits/underscore/hyphen, and a
literal is 1–64 bytes. One immutable invocation scalar and distinct literal
equalities prove that at most one partition runs, even while its value is
unknown. Duplicate equalities can overlap and are charged together. Independent
selectors or unsupported output expressions get no exclusivity credit.

`when: "{{inputs.parameters.partition}} == 0"` is deliberately **not** a proof:
raw substitution inserts untrusted output into an expression's source. Such
guards retain conservative summed demand; Oberth never rewrites them into the
safe form. Repositories must adopt the safe form themselves. Unexpected scalar
text cannot enable several safe equalities; missing or unbound inputs earn no
partition credits.

Cleanup dependencies may use an exact same-task terminal-state OR group such
as `(driver.Succeeded || driver.Failed || driver.Errored)` (optionally including
both `.Skipped` and `.Omitted`). These establish **ordering only**. Only a
matching delete's required **Succeeded** status can end a VM's lifetime before
a later create. Failed, errored, skipped, omitted or unproven conditional
deletes earn no release credit. A three-state group also proves its predecessor
ran; a five-state group may accept an omitted phase and cannot carry that proof
forward. If these paths fail to establish a partial order of lifetimes, sizing
retains the sum instead of inventing transitive deletion credits.
The failure-cleanup DAG and cancellation
`onExit` are unchanged. There is no phase-based early release to the scheduler:
it holds the admitted peak for the whole workflow, as before.

The server also enforces limits immediately before each VMI create. It changes
the resource Pod's pinned Argo executor command from create to get, and performs
the create itself before delivering the launcher token. The public, immutable
Workflow quota binding pins the repository ID, admitted grant/ceiling and peak.
A namespace-wide public ConfigMap, `oberth-vmi-reservations`, serializes pending
claims with Kubernetes resourceVersion checks across server restarts and
concurrent controllers. Both actual VMIs and pending creates count; an
unattributable legacy VMI is charged conservatively. An over-grant create is
refused before the API POST or launcher-token delivery. Existing grants and
defaults are unchanged; running Workflows retain their admitted grant snapshot,
including after revocation, consistently with existing adoption behavior.

Live accounting uses current CPU topology and the larger of guest memory and
memory requests, never hotplug maxima (`maxSockets`/`maxGuest`). A valid exact
claim is a floor for every readable dimension. Missing fields can use that
claim as a fallback. On a repository-labeled VMI, a **present, unreadable CPU
count** (including a string, fraction, null or malformed CPU parent) cannot
borrow a smaller claim: it consumes the repository's full grant, plus any larger
readable dimensions.
Other repositories' labeled objects remain isolated from this accounting.

An **oversized unlabeled VMI** is charged to every repository at its readable
size. If it exceeds the accounting maximum, it blocks **all new VMI creates
across repositories** until an administrator removes or repairs it. This is
intentional fail-closed behavior for an object whose owner cannot be attributed.
Only an administrator can introduce such an object; repository identities
cannot create, update or patch VMIs directly.

The chart pre-creates `oberth-vmi-reservations` as a metadata-only public
ConfigMap with `helm.sh/resource-policy: keep`. Oberth initializes its data once
with a resourceVersion check and a server-owned initialization marker. Helm
never owns the mutable data or marker, so later upgrades/rollbacks cannot replay
an empty reservation set. Missing or corrupt initialized data fails closed.
Retain this ConfigMap across restarts and rollbacks. Never use Helm
`--force-replace` or delete `oberth-vmi-reservations`: replacing/deleting it
loses pending reservation state.
The server's separate Role grants VMI create/get and only fixed-name ledger
get/update; it has no ConfigMap create permission. The frozen Argo Role stays
unchanged. No hostPath, new Pod privilege or pipeline create permission is added.

**Drain legacy resource-based VMI workflows before upgrading.** Adoption now
refuses a VMI Workflow without a server quota binding immediately. Old
resource-create Pods cannot obtain new launcher tokens. See the
[release note](releases/v0.17.59-kubevirt.md).

Each durable operation permits only one API POST, without automatic retries.
The operation has a 20-second context and the POST sends a 20-second API request
timeout. Completion saves its UID with an independent ten-second context even
if the caller was canceled. Reservation also adopts a live UID when name,
namespace, repository, source hash, operation and Workflow owner match, so a
lost response or failed completion update cannot permanently leak a slot.
Confirmed deletion releases that capacity. Closed receipts are pruned only
after their resource Pod is gone.

Pending claims carry their reservation time. When no matching live VMI exists,
a pending claim closes once its resource Pod UID is absent from a complete API
list **and** it is more than five minutes old, well beyond the bounded POST.
This also heals a crash between reservation and POST. Reconciliation happens
on the next reservation, not a background timer: after deleting a defunct
Workflow and allowing its resource Pods to disappear, the next reservation
reclaims dead claims once that five-minute margin has elapsed. A missing timestamp starts
a fresh grace period; a recent claim, a still-present Pod or an incomplete list
never earns an age-based refund. Timeout/NotFound alone never releases a slot.
A canceled stale observer cannot send another POST, and a pruned operation must
still prove its current resource Pod UID exists before reserving again.

Malformed labeled VMIs conservatively exhaust only their own repository's
available grant. An unattributed unreadable legacy VMI is charged the individual
admission maximum (1 VM, 4 cores, 8Gi) against each repository. Conflicting
pending operations remain charged to their repository and refuse only their
own retries; unrelated repositories continue. The ledger remains bounded to
2048 claims / 1 MiB. API/ledger errors and incomplete VMI inspection fail closed.
The guard covers repository-authored Argo VMI resources; the separate
administrator-controlled protected runner transport retains its existing policy.

## Direct kernel boot

`spec.domain.firmware.kernelBoot` boots a kernel and initrd from a container
image instead of the disk's bootloader:

```yaml
domain:
  firmware:
    kernelBoot:
      kernelArgs: console=ttyS0 root=/dev/vda1 ro
      container:
        image: <allowlisted prefix>/kernel@sha256:<64 hex>
        kernelPath: /boot/vmlinuz
        initrdPath: /boot/initrd.img
```

The image follows the containerDisk rules: digest-pinned and under an
`argo.containerDiskPrefixes` entry. `kernelPath` is required and `initrdPath`
optional; both must be absolute paths of at most 256 bytes made of
`[A-Za-z0-9._-]` segments, with no `.` or `..` segment. `kernelArgs` is
optional, at most 512 bytes, and made of single-space-separated words in
`[A-Za-z0-9._,:=/+-]`. `imagePullSecret`, `imagePullPolicy` and every other
firmware field (bootloader, serial, UUID, ACPI) are refused. A containerDisk
volume is still required for the root disk.

## Cleanup

Oberth's default terminal cleanup deletes leftover VMIs using the run's label,
name prefix, Workflow owner UID and an observed UID precondition. This backstop
is best effort: failures are audited while the run becomes terminal. Without
the original Workflow UID, it cannot prove ownership and leaves objects alone.

To require cleanup before PASS, opt into the terminal verdict:

```yaml
metadata:
  annotations:
    oberth.ci/cleanup-verdict: "30"
```

The value is a total deadline in seconds, from 1 to 300. This requires the
repository's KubeVirt grant. After Argo completes, Oberth records an
`oberth-cleanup/cleanup-verdict` hook result and writes its progress and object
list to the step log. It deletes run-created VMIs, DataVolumes and PVCs with
UID preconditions and waits for complete absence readback. Remaining objects,
foreign replacements, missing ownership proof, API errors or incomplete lists
fail the hook and the run. Recovery applies the same gate before reporting a
terminal result. Polling backs off exponentially from 100ms to a 2s cap,
within the declared total deadline. A clean verdict preserves an earlier
pipeline failure.

Cleanup includes Workflow-owned volumeClaimTemplates and source PVCs. PVCs
owned by a DataVolume must prove that parent's UID and Workflow ownership.
Inspection is bounded to 2,048 namespace objects per resource type, with
pagination; exceeding the bound fails closed. Same-prefix objects with
unproven ownership are listed as leftovers and never deleted. The hook name
`oberth-cleanup` is reserved when this annotation is present.

Explicit resource deletes still allow early phase release. They pass through
the server's token barrier: Oberth verifies the Workflow owner UID, checks the
authored UID when present, sends a UID-precondition API delete, and waits for
absence across controller ticks. Each API attempt has a 20-second bound; a
slow guest or storage finalizer keeps the step pending until absence or the
Workflow deadline. The pinned Argo main container then runs
`argoexec version`; it receives no delete permission and cannot repeat a
name-only delete against a replacement. A missing target succeeds. Flags,
force deletion and arbitrary selectors remain refused.

For example, pass the create step's UID output as the delete template's `uid`
input, then use:

```yaml
- name: delete-vmi
  inputs:
    parameters:
      - name: uid
  resource:
    action: delete
    setOwnerReference: true
    manifest: |
      apiVersion: kubevirt.io/v1
      kind: VirtualMachineInstance
      metadata:
        name: "{{workflow.name}}-amd64"
        uid: "{{inputs.parameters.uid}}"
        labels:
          oberth.ci/workflow: "{{workflow.name}}"
```

The same metadata-only, UID-pinned delete is admitted for
`cdi.kubevirt.io/v1beta1` `DataVolume` and `v1` `PersistentVolumeClaim`.
DataVolume deletion uses foreground propagation. This adds no DataVolume/PVC
create or get templates, disk cloning, external volume references, or pipeline
storage permissions. Existing creation admission remains in force. Drain
running resource-delete workflows before upgrading, since legacy Pods lack
the server delete guard and the launcher account no longer has delete access.
