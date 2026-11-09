# Automatic infrastructure retries

Oberth queues one fresh attempt when an eligible branch run fails in the
`infrastructure` or workflow-level `job` class accepted by `run_retry`, without
a failed step. Every failed step excludes automatic retry, including steps whose
failure is classified as infrastructure. This includes a trusted-policy-refresh
deadline, a lost executor after cleanup, and a workflow-level failure without a
failed step. An execution deadline qualifies only before any step started or
passed; a hung suite never receives a second execution window. The original
attempt keeps its failed status and error; the retry gets a new run ID at the
same repository, ref and SHA.

The retry inherits the original admission sequence, queued time and priority,
including an accepted priority request, administrator bump and aging. Its unique
history sequence remains new. Later queued work does not move ahead merely
because an infrastructure attempt failed. Existing priority classes, admission
limits and protected-work rules still apply. A newer ordinary push supersedes
stale work; an automatic retry cannot displace it.

The existing message/reason line records:

```text
automatic retry of <id> after infrastructure failure: <reason>
```

The same text and source/retry IDs are recorded in `run.retry.auto`, along with
attempt 1, source error, admission sequence and priority. Failure, retry, lineage
and audit commit in one writer transaction. An audit/enqueue failure creates no
partial successor. Existing statuses and presentation remain unchanged.

## Administration

The Helm setting is enabled by default:

```yaml
autoRetryInfrastructure: true
```

Set it to `false` to disable automatic infrastructure retries. The server flag
is `--auto-retry-infrastructure=false`; its default is true. This is a boot-time
administrator setting, applied by the normal server rollout. The budget is
fixed at **one retry** (two total attempts); there is no setting to increase it.
Disabling it affects future terminal decisions, not an already queued successor.
Manual `run_retry` remains available under its existing authorization rules.
It creates a fresh manual request at the normal queue position, with the
requesting actor and ordinary presentation. It does not inherit the automatic
retry reason or lineage; that new request may itself receive one eligible
automatic retry.

Automatic retry never applies to a user/admin cancellation, test or step failure,
promotion, plan, apply, release/tag run, or an attempt whose automatic retry
budget was already used. The spent budget survives restart recovery and the
existing `run_rerun` replay lineage. An explicit `run_retry` or a separate new
push starts a new work item.

A manual `run_rerun` in that lineage keeps the operator's reason. Its existing
reason line reads `rerun of <id> (lineage: automatic retry of <origin>): <reason>`;
omitting the reason leaves just the rerun and lineage. The `run.rerun` audit
entry retains the operator's exact reason, and the separate immutable
`run.auto-retry-lineage` entry retains the spent automatic budget. Reopening the
database or recovering the run after a restart does not reset that budget.

All retained progress attempts, terminal observations and persisted step rows
are checked for failed steps, regardless of whether the run or step is
infrastructure-classified. For an execution-deadline failure, any evidence of a
started or passed step also prevents retry, including an earlier attempt whose
latest state is queued. Started or passed steps alone do not prevent retry of a
non-deadline workflow failure. Missing or corrupt progress history does not
authorize replay. The original Workflow must be terminal, never submitted, or
settled through ownership-checked cleanup. Ambiguous creates and failed cleanup
stay owned by existing recovery and cannot start another executor prematurely.
Uncleaned trusted preflights, pending cancellation or any publication also block
automatic retry. Publication failures use the existing publication retry path.
A fresh attempt re-earns current policy, credential and resource admission.

## Upgrade behavior

This expands the previous pre-execution-only retry policy and is **on by
default** when the new setting is absent. Administrators who want manual-only
infrastructure recovery should include `autoRetryInfrastructure: false` in their
reviewed upgrade values. The chart handles an absent value during `--reuse-values`
upgrades and preserves an explicit false value.

No migration is added: the existing admission sequence, reason and audit lineage
are reused; the base schema remains 53. Legacy automatic-retry reasons remain
readable and retain their spent budget. Existing terminal runs are not scanned
or replayed on upgrade; only newly finalized eligible failures are considered.
Other restart and publication recovery mechanisms keep their existing contracts.
See [standalone configuration](standalone-configuration.md) for upgrade values
and [recovery semantics](rca-run-recovery.md) for the existing run-list behavior.

## Assisted continuation

A retry hold ([assisted continuation](assisted-continuation.md)) never
compounds with the automatic infrastructure retry: the injected in-Workflow
`retryStrategy` retries only a command that exited nonzero, a deadline-killed
or evicted attempt is never held, and a hold that expires is a retained failed
step, which this retry refuses as before.
