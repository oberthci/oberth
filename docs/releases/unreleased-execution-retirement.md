# Execution capability retirement

Oberth runs and gates Argo Workflows. Resource objects, their lifecycle and
cleanup, and workload test gates now belong to repository Workflows using
[operator-owned workflow capabilities](../workflow-capabilities.md).

This breaks the MCP API: `run_kvm_preflight`, `kubevirt_grant`,
`kubevirt_revoke`, `kubevirt_grant_get` and `kubevirt_grant_list` are removed.
Use `workflow_capability_grant`, `workflow_capability_get` and
`workflow_capability_revoke` for explicitly granted generic access.
The direct execution CLI commands and flags are removed.

Before upgrading, back up the database, drain old runs, confirm external
resource cleanup, and validate replacement repository Workflows. Existing
execution, capacity and grant rows remain as read-only historical evidence;
the upgrade neither claims cleanup nor converts old grants into new authority.
Migrations 45/46/47 retire the pilot, add an empty capability table and retire
execution journals. Combining unshipped migration branches requires assigning
unique contiguous versions before release. Rollback requires the pre-upgrade
database restore, as described in [backup and restore](../backup-restore.md).

Reused `vm`, `trustedTests` and retired Argo execution values are accepted and
ignored with a warning. The instance connector also moves outside Oberth;
transfer its ownership before upgrading a release that used it. Retained chart
values do not enable those removed features.
