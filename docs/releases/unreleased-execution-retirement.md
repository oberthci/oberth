# Unreleased: remove technology-specific execution (#1294 stage 3)

**Breaking MCP change:** the registry now exposes 75 tools instead of 80.
`run_kvm_preflight`, `kubevirt_grant`, `kubevirt_revoke`, `kubevirt_grant_get`,
and `kubevirt_grant_list` are removed, along with `oberth kubevirt`, the retired
execution flags and the retirement census command. Generic
`workflow_capability_grant`, `workflow_capability_get` and
`workflow_capability_revoke` remain.

Oberth runs and gates Argo Workflows. VM admission/budgets, device injection,
trusted preflight, the protected VM executor, guest harnesses, image publishing,
launcher diagnostics and execution-journal APIs are removed. Pipelines own
resource creation, their harnesses, cleanup and gates through operator-owned
workflow capabilities. Ordinary workflows, resource admission, release/WIF
validation, nonroot leaves and branch placement remain.

Legacy Helm settings are accepted but ignored, with upgrade warnings. Legacy
workflow declarations fail with a message naming the workflow capability grant.
Old runs still list and render. Active retired executions fail clearly at
restart; external resources need operator cleanup. Drain them before upgrading.
The upgrade resource inventory needs cluster-wide list permission on KubeVirt resources.
Existing migration SQL is unchanged and no migration is added. Retired tables
are inert until a later removal migration; restore the pre-upgrade database for
rollback. See [migration instructions](../workflow-capabilities.md#migration-from-retired-execution).
