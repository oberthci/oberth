# Staged workflow capability migration

Oberth adds [operator-owned workflow capabilities](../workflow-capabilities.md)
without deleting the deprecated VM execution path. Existing `OBERTH_KVM`,
`OBERTH_GUEST_TEST`, guest-test and `.oberth/tests.yaml` release gates, CLI flags,
MCP tools, chart RBAC, cleanup and preflight recovery remain functional.
KubeVirt grants retain their bounded authority and remain revocable; they never
become generic capability grants. Operators provision new capability accounts,
RBAC and resource capacity before migrating their pipelines.

Migrations 46/47/48 retire only the inactive pilot, create an empty capability
table, and reserve the deferred execution-retirement boundary. Schema 48 itself
does not delete, freeze or require draining live work; the v0.17.84 CLI requires
legacy executions to be drained and their resources deleted before replacement
(see the [retirement upgrade procedure](../retirement-upgrade.md)). A Recreate upgrade reaches
normal owner recovery with all execution journals, capacity interlocks and
pending cancellations intact. Existing Workflows are adopted or cleaned through
the same ownership checks as a restart; migration itself never proves cleanup.

Startup cancellation cleanup holds new dispatch and readiness until deletion is
observed. Failures keep the process alive, with two-minute attempt budgets and
retry delays from 250ms up to five seconds. Logs report `recovery_pending=true`
and `ready=false`; readiness reports `scheduler recovery is pending`. Restore
Kubernetes API access or the configured cleanup permissions if this persists;
the durable obligations and interlocks remain in place during retries. Shutdown
cancels the wait. Trusted-preflight cleanup retains its existing bounded recovery
and journal/capacity protection. Starting with v0.17.84, the CLI additionally
refuses an upgrade while legacy preflight Workflows or Oberth-owned VMs remain.
Drain those executions with the existing server before replacing it; ordinary
non-legacy Workflows retain restart adoption. See the
[retirement upgrade procedure](../retirement-upgrade.md).

The v0.17.82 database is at schema 39. The small v0.17.83 adds the run-list
covering index at 40. Migrations 41–45 persist bearer token
expiry, issue metadata revisions, publication abandonment, restart timing and
tag publication identity, respectively. Migration 49 adds independent numeric
resource ceilings. Submission priority retains the v0.17.82 implementation.
See the [complete v0.17.84 migration table](v0.17.84.md#upgrade-from-v01782-or-v01783).
Rollback requires the pre-upgrade database restore described in
[backup and restore](../backup-restore.md).

The v0.17.84 candidate includes the website and chart tunnel-connector
removal. Before upgrading, move connector ownership and the site pipeline
outside the chart. Existing `watchTunnel` values are accepted but ignored; they
do not retain chart ownership of the removed objects.

Move the connector to externally managed manifests and confirm its Pods serve
traffic. Remove any reference to the chart's `cloudflared` PriorityClass from that connector, or provision an
externally owned PriorityClass first. Confirm new connector Pods can be created
with the resulting class and that the website pipeline has an external owner
before upgrading the chart that removes these objects.

## Required release split

The v0.17.84 release is preparatory: it adds the live, read-only
`oberth admin retirement-check`, keeps legacy execution/recovery, and requires
the connector/site handoff on the relevant upgrades. Retirement inventory never
runs at server startup. Upstream outages, archived repositories and new legacy
branches refuse only a requested retirement verification.

Final execution removal is excluded from this release. A later
published release may remove the old engine after a published preparatory release
has been installed, dependent repositories have migrated, and the live drain,
cleanup and backup receipts are accepted. See [the release sequence](../legacy-removal.md).
The combined release preserves migrations 1–52, including publication policy
waiting (50), issue work status (51) and the final run-list index rebuild (52). This merge
adds no migration; new branch-specific migrations start at 53.
