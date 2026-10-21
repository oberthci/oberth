# Repository-authored KubeVirt VMIs

An administrator grants a registered repository the KubeVirt capability with
`kubevirt_grant` (MCP) or `oberth kubevirt grant <repo>` in the server pod.
`kubevirt_revoke` and `oberth kubevirt revoke <repo>` remove it. Each change
is audited. The grant alone does not permit arbitrary Kubernetes resources:
Oberth admits only bounded VMI manifests in named Argo `resource:` templates.

## Repository VMI budget

Each grant carries a budget across its admitted runs: the number of VMI create templates, their
summed `spec.domain.cpu.cores`, and their summed memory requests. A grant made
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
A run may also declare at most three VMI resource templates per VM in its
budget, for each VM's create, get and delete. A VMI's CPU request may not
exceed its cores, so the core budget also bounds what the VMIs reserve. The
budget counts guest memory; each virt-launcher Pod adds KubeVirt's own
overhead (a few hundred MiB) on top. The scheduler reserves each run's declared
VM count, cores and memory together with its ordinary admission weight. Two or
more runs proceed concurrently when their total fits the repository grant.
A run that would exceed the remaining capacity stays queued with reason
`KubeVirt capacity`, keeps its FIFO place, and holds no weight or concurrency
group until capacity frees. A single run exceeding the grant still fails
admission. Completion, cancellation and supersession release the reservation
once execution is terminal or Workflow deletion is accepted.

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
gives it a VMI-only Role. The shared Argo executor account receives no VMI
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
8Gi; the repository budget above bounds their sum across admitted runs. Both are enforced at
admission. Credentialed workflows cannot contain VMI resource templates.

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
