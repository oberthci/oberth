# Workflow deadlines and delayed status

A repository's `spec.activeDeadlineSeconds` is retained on the submitted Argo
Workflow, capped by the administrator's `--argo-workflow-timeout` ceiling
(default 12 hours). An omitted deadline receives a 30-minute server default.
The repeatable administrator flag `--argo-repo-workflow-timeout
forge/org/repo=20m` caps that canonical repository independently; a repository
cannot raise its cap, and caps never raise the global ceiling. Explicit zero
or negative deadlines remain invalid.
Oberth's branch CI declares 5400 seconds. Retrying an ambiguous submission
adopts the same object; it does not recreate the Workflow or reset its clock.

Argo v4.0.8 calculates its workflow deadline from `status.startedAt`, truncated
to a whole second, plus that submitted budget. This is the Argo Workflow start,
not Oberth's workflow admission grant, source preparation, or the time a status
client begins waiting. API creation time is shown separately. A missing Argo
start timestamp means the deadline instant and remaining time are unknown;
creation/admission timestamps are not substituted. Repository-authored
workflow and template suspension are refused.

Templates may also declare `activeDeadlineSeconds` and `timeout`; these fields
survive submission (a literal template deadline receives the start-up
allowance described below). Argo constrains ordinary step Pods by the workflow time
remaining and any shorter template deadline. Every step, including VM resource steps, shares the bounded workflow lifetime.
A server watchdog uses Argo's start timestamp and adds a grace period of
`max(2 minutes, 10% of the workflow budget)`. This lets Argo record its deadline
failure, failed steps and diagnostics before the watchdog requests
ownership-checked deletion. Before Argo reports a start, the watchdog uses
Workflow creation with the same budget and grace, bounding an unstarted
Workflow too. Once the start appears, the watchdog switches to that clock.
Restart and retry retain the observed timestamps and do not restart the budget.
An initial failed read uses the existing bounded read-outage policy until the
submitted deadline can be read. Individual commands may have shorter bounds.

Detailed MCP `status` / `run_get`, the run-detail API, `oberth run`, and the
dashboard's run detail expose an ephemeral execution observation, bounded by
the existing two-second observation budget. It remains available after steps
have progressed. The observation uses one exact Workflow GET and at most one
scoped Pod LIST of 16 items, with ownership checks; it does not read logs.
Run-list and live-log refreshes do not gain execution polling.
`Scheduling.execution.server_deadline` shows the independent watchdog with
`clock: workflow_started_at` after Argo records a start timestamp and
`clock: workflow_created_at` beforehand. Its budget includes the grace period.

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
`reason: DeadlineExceeded`, and `display_reason: Execution deadline exceeded.`
(the display status stays `running` until the store finalizes the run, then
`failed`; the deadline is a reason, never a status of its own).
Exit 143 alone does not establish the cause. Free-form controller text stays
withheld; only fixed vocabulary and timestamps are returned. The canonical
terminal error preserves the deadline presentation after durable finalization,
including retained exact controller messages from earlier versions.

The durable run can still say `running` while the execution observation says
`Failed` and `deadline_exceeded`: final logs, diagnostics, cleanup or database
finalization may lag behind execution. The display reason is not a durable
terminal result. Long-poll completion and publication safety use recorded
terminal state and evidence.

For run `81745dce`, operator evidence confirmed that test-store/test-rest
received exit 143 at 10:33:40Z, three seconds after Workflow creation
09:03:37Z + 5400 seconds. The reported two-hour running interval was a stale
control-plane display, not missing deadline enforcement. The new clock and
lag fields make that distinction visible without changing execution.

Trusted preflights use the same document/default/repository/global limits,
with an additional six-hour hard ceiling. Their journaled absolute deadline
starts at preflight admission and covers source producers, trusted suites and
init containers. The API and dashboard expose it as `execution.deadline` with
`clock: preflight_admitted_at`. A server deadline failure records a failed run
and releases its workflow admission slot even if Kubernetes cleanup remains pending.
`run_cancel` releases preflight admission before requesting deletion of the
journal-bound preflight Workflow and its owned Pods. It reports the preflight
as `deleted` when deletion requests were accepted, or `pending` on a failure;
accepted deletion does not promise that Pod finalizers have finished.
The VM journal retains its separate capacity slot and cleanup obligation until
absence is confirmed, preventing an unobserved VM from being treated as gone.
A separate background task checks terminal preflight journals every five seconds after
their local execution owner returns. It retries ownership-checked cleanup and
absence readback with a two-minute budget per attempt, so eventual object
removal frees the VM slot without a server restart. Deletion errors remain
pending without stopping the scheduler. Cleanup does not retry the run or
change its terminal result, and never targets a running parent.
The executor identity init container logs its initial wait, progress every 30
seconds and receipt, without logging credential contents.

## Step deadlines exclude Pod start-up (#1499)

Kubernetes counts a Pod's `activeDeadlineSeconds` from Pod start, so image
pulls, init containers, kubelet contention and the post-exit wait sidecar are
charged to the step. On a loaded single node, steps that finished in 39 seconds
were killed at a 180-second limit. Oberth therefore treats a template's literal
`activeDeadlineSeconds` as the budget of the step command and submits the Pod
with a bounded start-up allowance on top:

    pod deadline = declared + clamp(declared / 2, 120s, 600s)

so 60s becomes 180s, 180s becomes 300s and 3600s becomes 4200s. The declared
value is recorded on the step Pod as the server-owned annotation
`oberth.ci/step-deadline-seconds`; admission refuses a repository-authored
copy. The workflow deadline is unchanged and still bounds the whole run, and
Argo still lowers a Pod deadline to the remaining workflow budget.

When a step Pod is killed by its own deadline (`DeadlineExceeded`), Oberth
reads the owned Pod's `main` container:

- `main` exited 0: the step succeeded; the kill happened outside the step
  command. It is never reported as a step deadline failure.
- `main` never started (still `ContainerCreating` or `PodInitializing`, or
  rewritten by the kubelet to `ContainerStatusUnknown` with exit 137, no
  container and no start time), or the Pod deadline stopped `main` (it
  finished at the Pod deadline) before it ran the declared budget recorded
  beside the exact floored Pod deadline: start-up consumed the allowance.

A `main` container that exited nonzero on its own before the Pod deadline, was
killed for memory, or never started because of an image or configuration error
(`ErrImagePull`, `ImagePullBackOff`, `InvalidImageName`,
`CreateContainerConfigError`, ...) is the step's own failure and opens a CI-red
issue as usual.

Both are infrastructure failures with a reason that starts with
`Startup deadline:` and names the step, the main runtime and the limits. They
use the existing single automatic infrastructure retry (`infra-retried`,
`retried as <run>`), open no CI-red issue, and keep their step rows and logs.
A `main` container that ran at least its declared budget remains the step's own
`Execution deadline exceeded.` failure, as does any workflow-deadline kill or a
Pod whose deadline Argo lowered to the remaining workflow budget. Pods
submitted before the upgrade carry no annotation; only the exit-0 rule applies
to them.
