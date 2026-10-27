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
Workflows without the server quota binding retain summed demand.
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
gives it a VMI get/list/watch/delete Role; it cannot create VMIs. The shared Argo executor account receives no VMI
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
required. Only those two substitutions are permitted. The manifest must be
one YAML document with no carriage returns. Create manifests are limited to
the admitted containerDisk, inline cloud-init, emptyDisk, pod networking,
masquerade interface, CPU, memory, architecture and kernelBoot fields. Get
and delete manifests carry metadata only. Each VMI is limited to 4 cores and
8Gi; the repository budget above bounds concurrency across admitted runs.
Admission and the server create guard both enforce the budget. Credentialed workflows cannot contain VMI resource templates.

## Proven VM phases and the create guard

A resource create task finishes while its VMI is still alive. A dependency on
that create alone therefore earns no concurrency discount. Oberth counts each
create invocation separately, including repeated use of one template or VM
name. Two invocations are non-overlapping only if a matching explicit delete
is a descendant of the earlier create **and an ancestor of the later create**.
Exit-handler cleanup never proves a barrier. Deletes use Argo's normal waiting
semantics; authored resource flags and success/failure conditions remain refused.

For bounded unconditional DAGs and steps, including nested named templates,
Oberth computes the maximum concurrent count, cores and memory independently
from these lifetime relations. For example, two parallel 2-core/6Gi VMs followed
by serial create/use/delete phases reserve 2 VMs, 4 cores and 12Gi, even when
four different create templates exist. Cycles, unknown references, conditions,
fan-out, retry policies, lifecycle hooks, unsupported dependency expressions,
exit-handler creates, or expansion beyond 256 nodes fall back to the existing
sum of declared create templates. There is no phase-based early release to the
scheduler: it holds this peak for the whole workflow, as before.

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

The server gets a separate Role with VMI create/get and permission to create
the ledger ConfigMap. Kubernetes cannot name-scope create, but get/update are
restricted to `oberth-vmi-reservations`; audit-anchor update is not granted.
The frozen Argo Role is unchanged. No hostPath, new Pod privilege, or pipeline
ServiceAccount create permission is added. Drain old VMI workflows before
upgrading: their old resource-create Pods cannot receive new launcher tokens.

Each durable operation permits only one API POST, with automatic POST retries
disabled. An explicit API rejection closes its claim; an ambiguous transport
failure, timeout or cancellation keeps the slot even if a subsequent GET sees
nothing. A matching observed VMI can resolve the pending claim. Only absence
of a previously confirmed UID releases its capacity; closed-operation receipts
are pruned after their resource Pod is gone. The ledger is bounded to 2048
claims/1 MiB; inspection is bounded and errors refuse further creates.

If an ambiguous claim has no observable VMI, inspect the ledger and API audit
records before recovery. Quiesce the server and its outstanding API requests,
verify the operation cannot still commit, and have an administrator remove
only that verified-stale claim before restarting. Never delete the entire
ledger or refund a pending claim based solely on a timeout/NotFound. This
conservative case can hold capacity until operator recovery. The guard covers
repository-authored Argo VMI resources; the separate administrator-controlled
protected runner transport retains its existing policy.

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

Oberth's run cleanup owns VMI deletion; a workflow needs no explicit delete
step. After a run completes, fails or is cancelled, Oberth lists VMIs with the
run's Workflow label and deletes survivors whose names have that Workflow
prefix, using observed UID preconditions. A VMI that is already gone, or was
never created because the run failed first, is not an error, so a run that
fails before its VM exists finishes with a clean cleanup. The Workflow owner
reference removes VMIs too, when the Workflow itself is deleted.

An exit-handler delete template remains admitted for releasing VMs before the
run ends. It is safe when the VMI never existed: Argo's resource executor
(v4.0.8) always runs `kubectl delete --ignore-not-found`, so Oberth still
refuses resource `flags`. The KubeVirt installation and cluster reachability
policy are managed separately from this admission and identity increment.
Cleanup is best effort: failures are logged and audited, while the finished
run still becomes terminal.
