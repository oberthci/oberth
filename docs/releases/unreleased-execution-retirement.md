# Staged workflow capability migration

Oberth adds [operator-owned workflow capabilities](../workflow-capabilities.md)
without deleting the deprecated VM execution path. Existing `OBERTH_KVM`,
`OBERTH_GUEST_TEST`, guest-test and `.oberth/tests.yaml` release gates, CLI flags,
MCP tools, chart RBAC, cleanup and preflight recovery remain functional.
KubeVirt grants retain their bounded authority and remain revocable; they never
become generic capability grants. Operators provision new capability accounts,
RBAC and resource capacity before migrating their pipelines.

Migrations 45/46/47 retire only the inactive pilot, create an empty capability
table, and establish a guarded upgrade boundary. Live execution journals and
capacity remain writable. The upgrade refuses VM work in flight; use the old
binary to finish cleanup, then retry with a database backup. Store startup also refuses every running run and pending cleanup obligation,
conservatively covering resource Workflows without a VM journal row. The
migration cannot consult Kubernetes; existing runtime cleanup and RBAC remain. Existing grants alone
do not block this staged upgrade because their runtime and permissions remain.
Schemas 39–44 are the real predecessor migrations from priority, integration
and replay-C; this branch carries their schemas to keep the ledger contiguous.
Rollback requires the pre-upgrade database restore described in
[backup and restore](../backup-restore.md).

The website and tunnel removal is a separate final commit. Hold that commit
until connector ownership and the site pipeline have moved outside the chart.
No deployment values are changed by this work. Merging the earlier staged
capability commits alone preserves existing chart-managed website/tunnel use.
