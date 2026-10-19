# Repository-authored KubeVirt VMIs

An administrator grants a registered repository the KubeVirt capability with
`kubevirt_grant` (MCP) or `oberth kubevirt grant <repo>` in the server pod.
`kubevirt_revoke` and `oberth kubevirt revoke <repo>` remove it. Each change
is audited. The grant alone does not permit arbitrary Kubernetes resources:
Oberth admits only bounded VMI manifests in named Argo `resource:` templates.

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
masquerade interface, CPU, memory and architecture fields. Get and delete
manifests carry metadata only. The per-VM and per-run budgets are enforced at
admission. Credentialed workflows cannot contain VMI resource templates.

The Workflow owner reference normally removes VMIs. After a run completes or
is cancelled, Oberth also lists VMIs with the run's Workflow label and deletes
survivors whose names have that Workflow prefix, using observed UID
preconditions. The KubeVirt installation and cluster reachability policy are
managed separately from this admission and identity increment. This cleanup is
best effort: failures are logged and audited, while the finished run still
becomes terminal. The Workflow owner reference remains the primary cleanup
mechanism.
