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

The file is read from the **default branch**, so changing a schedule is a
commit and a review like any other change.

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

**A fire is skipped when the repository already has a run queued or running**,
and the skip is recorded. A periodic health signal gains nothing by queueing
behind another run, and on a single node one heavy build already saturates
memory. A skip does not advance the schedule, so the entry fires on the next
tick once the repository is idle.

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

A failing scheduled run opens the same CI issue a failing pushed run does, and
closes it on recovery. Routing that somewhere is a separate concern.

## Limits

| Setting | Default | Meaning |
|---|---|---|
| `--schedule-min-interval` | 15m | Shortest interval a repository may ask for |
| `--schedule-max-entries` | 8 | Most entries one repository may declare |

Both exist because a schedule is the first repository-authored input that causes
work with nobody watching. Without a floor, `* * * * *` is a denial of service
written in five characters.
