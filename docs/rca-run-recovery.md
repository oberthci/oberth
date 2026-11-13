# Run recovery and history

Infrastructure failures remain failed runs with the observed cause and an
appropriate retry hint. Oberth does not create automatic replacement runs.
Use [native Argo template retries](infrastructure-retries.md), or manual
`run_retry` / `run_rerun` after the failure. These manual actions retain their
existing ref, credential, publication and cleanup safety checks.

An uncertain Workflow create/Stop remains owned until its exact external
outcome is established. Cleanup can recover after a server restart; it never
permits another execution to bypass unresolved cancellation or publication.
Running Workflows are adopted by exact run, Workflow UID and spec identity.
A claimed run without Workflow intent can resume preparation at its existing
acceptance position. Historical audit receipts remain immutable evidence.

Run lists show accepted work, then running work, then terminal history. The
public display vocabulary is `queued`, `running`, `passed`, `failed`,
and `cancelled`. Native Argo/Kubernetes waiting appears as the
queued reason; it does not create a new status. Runs remain
addressable by their durable IDs. Scoped readers retain repository isolation.

Readiness observes durable state in the background with bounded read budgets;
a stale or failed observation cannot claim that the installation is idle.
See [restart handover](restart-handover.md) and
[native scheduling](native-scheduling.md).
