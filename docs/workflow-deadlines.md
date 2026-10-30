# Workflow deadlines and delayed status

A repository's `spec.activeDeadlineSeconds` is retained on the submitted Argo
Workflow, capped by the administrator's `--argo-workflow-timeout` ceiling.
Oberth's branch CI declares 5400 seconds. Retrying an ambiguous submission
adopts the same object; it does not recreate the Workflow or reset its clock.

Argo v4.0.8 calculates its workflow deadline from `status.startedAt`, truncated
to a whole second, plus that submitted budget. This is the Argo Workflow start,
not Oberth's weighted admission grant, source preparation, or the time a status
client begins waiting. API creation time is shown separately. A missing Argo
start timestamp means the deadline instant and remaining time are unknown;
creation/admission timestamps are not substituted. Repository-authored
workflow and template suspension are refused.

Templates may also declare `activeDeadlineSeconds` and `timeout`; these fields
survive submission. Argo constrains ordinary step Pods by the workflow time
remaining and any shorter template deadline. There is no universal separate
Oberth per-step timeout. Individual commands can have their own bounds, such
as the branch lint watchdog and Go test timeout. This change adds no timeout,
watchdog, cancellation, resubmission or admission behavior.

Detailed MCP `status` / `run_get`, the run-detail API, `oberth run`, and the
dashboard's run detail expose an ephemeral execution observation, bounded by
the existing two-second observation budget. It remains available after steps
have progressed. The observation uses one exact Workflow GET and at most one
scoped Pod LIST of 16 items, with ownership checks; it does not read logs.
Run-list and live-log refreshes do not gain execution polling.

`Scheduling.execution.deadline` (`scheduling` on MCP status) includes:

- `budget_seconds`, `clock: workflow_started_at`, and known creation/start times.
- `expires_at`, `remaining_seconds`, and `elapsed` when the start is known.
- `execution.observed_at`, plus `finished_at` and
  `reconciliation_lag_seconds` when a terminal Workflow is observed while
  durable run finalization is still pending.

Remaining time and lag are snapshots at the observation time; a long-poll
response also carries observation age. The absolute deadline remains available
for clients that render a live countdown.

An elapsed clock alone yields `state: deadline_elapsed`: no deadline kill is
inferred. Recognized controller deadline messages or a current-attempt owned Pod's
`DeadlineExceeded` reason yield `state: deadline_exceeded`,
`reason: DeadlineExceeded`, and `display_status: deadline-exceeded`.
Exit 143 alone does not establish the cause. Free-form controller text stays
withheld; only fixed vocabulary and timestamps are returned. The canonical
terminal error preserves the deadline presentation after durable finalization,
including retained exact controller messages from earlier versions.

The durable run can still say `running` while the execution observation says
`Failed` and `deadline-exceeded`: final logs, diagnostics, cleanup or database
finalization may lag behind execution. The display label is not a durable
terminal result; long-poll completion, admission release and release/replay
safety continue to use the existing recorded state and evidence.

For run `81745dce`, operator evidence confirmed that test-store/test-rest
received exit 143 at 10:33:40Z, three seconds after Workflow creation
09:03:37Z + 5400 seconds. The reported two-hour running interval was a stale
control-plane display, not missing deadline enforcement. The new clock and
lag fields make that distinction visible without changing execution.
