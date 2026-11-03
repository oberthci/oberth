# Staged workflow capability migration

Oberth adds [operator-owned workflow capabilities](../workflow-capabilities.md)
without deleting the deprecated VM execution path. Existing `OBERTH_KVM`,
`OBERTH_GUEST_TEST`, guest-test and `.oberth/tests.yaml` release gates, CLI flags,
MCP tools, chart RBAC, cleanup and preflight recovery remain functional.
KubeVirt grants retain their bounded authority and remain revocable; they never
become generic capability grants. Operators provision new capability accounts,
RBAC and resource capacity before migrating their pipelines.

Migrations 45/46/47 retire only the inactive pilot, create an empty capability
table, and reserve the deferred execution-retirement boundary. Schema 47 does
not delete, freeze or require draining live work. A Recreate upgrade reaches
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
and journal/capacity protection. No admission-pause or manual drain is required
before this compatible upgrade.

Schemas 39–44 are the predecessor migrations from priority, integration and
replay-C. Submission priority uses the v0.17.82 implementation; schemas 40–44
remain carried here to keep the ledger contiguous.
Rollback requires the pre-upgrade database restore described in
[backup and restore](../backup-restore.md).

The website and tunnel removal is a separate final commit. Hold that commit
until connector ownership and the site pipeline have moved outside the chart.
No deployment values are changed by this work. Merging the earlier staged
capability commits alone preserves existing chart-managed website/tunnel use.

Before releasing the final removal commit, move the connector to externally
managed manifests and confirm its Pods serve traffic. Remove any reference to
the chart's `cloudflared` PriorityClass from that connector, or provision an
externally owned PriorityClass first. Confirm new connector Pods can be created
with the resulting class and that the website pipeline has an external owner
before upgrading the chart that removes these objects.
