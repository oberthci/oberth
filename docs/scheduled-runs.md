# Scheduled runs

A repository can run its pipeline on a clock as well as on a push.

Everything else Oberth runs is triggered by someone pushing, which misses the
class of failure that arrives without a commit: a dependency yanked, a CVE
published, a base image moved, a certificate expired, a flaky test finally
losing the coin toss.

## Declaring one

```yaml
# .oberth/schedule.yaml on the default branch
schedules:
  - name: nightly
    cron: "0 3 * * *"
    branch: main

  - name: weekly-deps
    cron: "0 4 * * 1"
```

`branch` is optional and defaults to the repository's default branch. A due
entry runs that branch's `.oberth/build.yaml` at its current commit, through the
same path a push uses: same admission gate, same fragments, same artifacts, same
failure issue.

A scheduled run is a `schedule` run for `oberth.ci/size-by-trigger` and the
`oberth-trigger` workflow parameter, so a pipeline can keep a heavy gate for
its nightly and promotion runs and leave it out of branch pushes (see the
README's scheduler annotations).

The file is read from the **default branch**, so changing a schedule is a
commit and a review like any other change.

## Selecting work by entry

`{{workflow.parameters.oberth-schedule}}` contains the resolved entry name
(`nightly` or `weekly-deps` above). For example:

```yaml
when: "'{{workflow.parameters.oberth-schedule}}' == 'weekly-deps'"
```

This is a reserved, server-owned workflow parameter. Reference it directly;
do not declare it in `spec.arguments.parameters`, including a default or
`valueFrom`. Such declarations are rejected at admission. Cron fires and
`schedule_trigger` both persist the resolved name before queueing. Retries,
reruns and restart recovery retain that original identity and commit, even
if the schedule file is edited or its entry removed. Ordinary branch, tag
and promotion runs receive an empty value when they reference the parameter,
and otherwise omit it. No caller parameter can turn them into a named
schedule run.

Named entries on the same repository/branch/SHA remain distinct queued runs.
Ordinary branch pushes and other entries are independent. A due cron occurrence
coalesces into a queued or running attempt of the same entry and branch instead
of superseding it. Other active work does not stop cron from queueing. Explicit
manual retries or forced triggers can still replace an attempt of the same
entry and branch. Resource admission and declared concurrency groups bound
execution.

Run list/detail APIs expose `ScheduleEntry`; `status` and `wait` expose
`schedule_entry`. The dashboard shows the name in the run stream and detail.
Historical runs created before this feature retain an empty/unknown identity;
it is never guessed from the current file.

## Times are UTC

There is no timezone field, and one is refused rather than ignored. `0 3 * * *`
is 03:00 UTC whatever the server thinks its local time is. A schedule that
silently shifts twice a year is worse than one that is always in a timezone you
have to convert from.

## What is refused, and how far the refusal reaches

Two tiers, on purpose.

**One entry is refused, its siblings still run**, when its expression will not
parse or when it fires more often than the operator allows. A typo in one line
should not silence a repository's other schedules.

**The whole file is refused, for that repository only**, when the server cannot
trust its reading of it: an unknown field, a duplicate name, a missing name or
expression, an unsafe branch, more entries than the cap, or an oversized
document. A file the server half-understands should not be half-obeyed.

Either way, other repositories are unaffected. That is a property the
implementation is tested against directly, because a single bad file silently
stopping every nightly in the estate is the failure worth designing out.

## The minimum interval is measured, not read

The operator sets a floor with `--schedule-min-interval` (default 15 minutes),
and an entry below it is refused by name.

The floor is enforced by asking the parser for the fires an expression actually
produces and taking the **smallest gap between any two**, not by looking at the
text. These are the same schedule written three ways:

```
* * * * *
*/1 * * * *
0-59 * * * *
```

and this one is not obviously either:

```
0,1,30 * * * *      fires at :00 and :01, one minute apart
```

Reading only the first two fires after midnight reports 29 minutes for that
last expression and lets it straight past a 15-minute floor. Measuring the
smallest gap reports one minute and refuses it.

## Expressions

Five fields, `minute hour day-of-month month day-of-week`, with `*`, a number, a
range `a-b`, a list `a,b,c`, and steps `*/n` or `a-b/n`. Sunday is `0` or `7`.

`@daily` and friends are refused, as is a six-field expression with seconds.
Both are refused by name rather than guessed at, because guessing what someone
meant is how a nightly quietly becomes hourly.

When day-of-month and day-of-week are **both** restricted they are OR-ed, which
is the long-standing cron convention: `0 0 1 * 5` fires on the 1st and on every
Friday, not only on Fridays that fall on the 1st.

## Overlap, downtime and attribution

**A due fire is queued even when other repository work is busy.** Weighted admission,
KubeVirt capacity and declared concurrency groups decide when it executes. A
stream of ordinary branch runs must not prevent a nightly from entering that
queue. `fired` means the run was enqueued, not that it has started or passed.
The current tick's branch head is pinned in the queued run; later pushes do not
replace a named schedule with an ordinary branch run.

If that entry already has a queued or running run on the target branch, the
occurrence is recorded as `coalesced`, preserving that run's original commit and
queue position. Coalescing advances the cursor to the tick time: that occurrence
is consumed, and the next one is the first cron time after it. Completion does
not trigger an extra catch-up run for consumed occurrences. This prevents a
nightly whose capacity wait or runtime exceeds its interval from killing itself
at every occurrence. The lookup covers all active runs, without a history-page
limit; different entries and target branches remain independent.

Older servers recorded `skipped` while any repository run was active. Those
outcomes did not advance the successful-fire cursor: pending entries catch up
on the next eligible tick, including after restart, even if the repository is
still busy. Failed enqueues and paused entries also retain that cursor. The
latest outcome and its time remain visible independently of the last successful
fire. Migration v32 (#1190) backfills surviving successful rows; entries whose
latest row was skipped, failed or paused have a pending cursor when their
successful-fire history is unavailable. They queue once when scheduling is
enabled and the entry is unpaused, then resume their normal cadence.

Manual `schedule_trigger` still refuses active repository work unless `force`
is set. That guard prevents an accidental extra manual run; cron occurrences
are already bounded by their consumed-occurrence cursor and minimum interval.

Cron checks the cursor snapshot and the active entry in the same SQLite write
transaction that records `fired` or `coalesced`. A new run, receive event, audit
record and cursor commit together. A lost enqueue reply or concurrent stale tick
cannot create a second run, including after the original run has finished.

An unreadable cursor aborts the repository tick, logs the error, and attempts to
record `failed_state` without inventing or advancing a cursor. An active-entry
query or enqueue error logs the error and records `failed`, preserving a due
cursor for retry. Outcome-write errors are logged too; an unreadable database
cannot be expected to persist its own failure. No read failure blindly enqueues.

**After downtime each schedule runs once, then resumes.** Never once per missed
fire: three days of downtime on an hourly schedule would otherwise mean 72 runs
competing with real pushes.

**A scheduled run is attributed to `oberth:scheduler`**, not to whoever pushed
last. No uplink identity caused it, and the audit chain should not imply one
did.

## Reading them

```
oberth schedules            # every repository, with next due time and last outcome
oberth schedules <repo>     # one repository, including why an entry was refused
```

Scheduled runs are marked in the dashboard's run stream.

A failing scheduled branch run opens or updates one CI issue per repository and
branch, even on a non-default branch, and green closes it on recovery. Projection
does not lock the issue. Ordinary pushed runs file issues only on the default
branch; promotions and tag/release runs file none. Routing that somewhere is a
separate concern.

## Limits

| Setting | Default | Meaning |
|---|---|---|
| `--schedule-min-interval` | 15m | Shortest interval a repository may ask for |
| `--schedule-max-entries` | 8 | Most entries one repository may declare |

Both exist because a schedule is the first repository-authored input that causes
work with nobody watching. Without a floor, `* * * * *` is a denial of service
written in five characters.
