# C10: Schedules

## Purpose

C10 reads repository schedules into admitted native CronWorkflows and verifies
the parent of each child Workflow. Argo owns firing, catch-up, overlap and
execution; Kubernetes places Pods. C2/C5/C7 attach children to ordinary history
without submitting again. C10 owns no firing clock, execution queue or run cap.

## Owned data and public contract

C10 owns `internal/schedule` (YAML and numeric cron parsing) and
`internal/cronsync` (native synchronization). [Shared Go declarations](README.md#shared-contracts)
define values; consumer ports inject Git, admission, native operations, the
repository catalogue and audited pause intent. All dependencies are required.

| Contract | Behavior |
| --- | --- |
| Declaration | Read `.oberth/schedule.yaml` at the default head: `schedules: [{name,cron,branch?,timezone?,concurrencyPolicy?}]`. Default branch/UTC fill omissions. Admit `.oberth/build.yaml` at each selected branch head, frozen until sync. Collect entry/field/value refusals and valid siblings together. Numeric five-field cron supports lists/ranges/steps, Sunday 0/7 and native day-of-month/day-of-week OR. Descriptors, seconds, named months/weekdays and Local timezone refuse. |
| Parent | One name `oberth-cron-<base36-repository-id>-<first-12-SHA256-bytes-of-entry-as-hex>`, ≤52 characters. Ownership requires exact name, literal entry parameter and managed-by label. Copy the sealed spec, validate natively, and update in place, retaining UID/status/active children. Ignore authored controller-instance selectors and use the installation label below. Parent metadata is literal. The sealed Workflow spec's `workflowMetadata` gains only the run label from `workflow.name`. Frozen labels and the ref annotation go in the CronWorkflow's `spec.workflowMetadata`, not in the sealed spec. |
| **Delta:** Operations | SyncAll, SyncRepository, RefsChanged, AuthorityChanged, SetPause, Status and Snapshot each finish their work synchronously under one cancelable lock. Callbacks must not reenter. The supplied context bounds lock and I/O; there is no component timer, pending queue, coalescing, retained caller failure or retained publisher. |
| Ref relevance | Unrelated refs return before locking/native LIST after a bounded declaration read. Default-head moves, entry branch moves and unreadable declarations are relevant; a relevant event syncs the whole repository. |
| Authority | Under that same lock, LIST and suspend every owned parent, then publish authority, then resync all. An incomplete LIST/suspension refuses without publishing and warns that some schedules may still run. A failed publisher leaves parents paused. No narrower barrier or known-name fallback; repeat the complete admin action to retry. |
| Pause | C7 owns latest audited schedule.pause/resume intent for canonical-repo#entry. Read it before creation, replacement-spec apply and user pause projection, including conflict retries. Native Suspend is user intent OR refusal. Unconditional safety suspension reads neither intent nor catalogue. Refusal detail stays in memory and is recomputed on startup, with no native suspension annotation. |
| Status | Status/Snapshot take an already-resolved Repository, read its intent and native facts without enumerating the catalogue, and expose entry/cron/zone/branch/name, user pause, sync suspension, error/detail, conditions, last and advisory next time. Suspended parents have no next time. Reads neither fire nor repair. |
| ChildBinding | Return shared nested CronRunBinding or permanent ChildRefusal, distinguishing API failure. Verify configured namespace, controller owner kind/name/UID, live managed parent UID, native child name, creation time, entry, CI tier and frozen ref/commit/trigger. Exactness covers the sealed spec and frozen facts, not arbitrary outer-object `oberth.ci/` metadata. Spec comparison normalizes only native `Shutdown` initialization. C5 binds the child's spec digest; C7 commits proof and run together idempotently. The parent may have advanced: use child facts. |

Every submitted CronWorkflow's metadata and its `spec.workflowMetadata` carry
`workflows.argoproj.io/controller-instanceid` with C9's installation instance ID,
so the parent and every resulting Workflow use that installation's controller.

A child run ID is its full native name, actor is oberth:scheduler and trigger is
schedule. A committed parent/child proof permits recovery after orphaning;
unproven orphans gain no authority. Preserve schedule_entry through rerun and
observation. A schedule consumes its own repository's source as a push of its branch;
tag changes re-admit it. Children load via locator `scheduled`, Keep on acceptance and
fail with the push reason if refused. Pins refuse: `scheduled source is not supported`.
Manual triggers use ordinary admission; force skips only explicit user pause.

**Delta:** C1/C2/C11 expose scoped schedule_list and /api/schedules with collection envelopes;
admin pause/resume commit intent before projection; trigger replies retain the
accepted run ID even if later work fails. No transport registry belongs to C10.

## Allowed dependencies

[dependencies.json](dependencies.json) lists shared model/native/Git/admission/
source/step values, the pure schedule parser and native clients. C3 supplies
prepared admission by callback; C10 calls only C4 native-schema validation.
Catalogue, pause and authority publication implementations remain injected.

## Invariants and budgets

1. Startup follows shared-watch readiness and syncs all; Argo fires during
   server downtime. Errors are shown and retried on the next relevant event,
   admin repair or startup. One lock serializes control-plane work, not runs.
2. **Delta:** Author refusals suspend an existing owned parent without replacing its spec;
   suspension must succeed before refusal converges. Git/admission/store/native
   outages and cancellation leave its last admitted spec and pause flag unchanged.
   Classify the whole wrapped/joined tree with publicerr.Permanent: a fixable
   refusal cannot hide an unavailable cause. Exact file absence alone removes
   a declaration; typed Git public errors unwrap to their public classifications.
3. Unreadable pause intent prevents create/replacement/user projection. A saved
   pause survives recreation/restart. Resume cannot clear a failed repository or
   full sync or authority refusal; successful re-admission clears it. SetPause
   verifies catalogue identity and reads intent once, even with no parent.
4. Delete only after complete catalogue/declaration/native observations prove
   removal/archive. Delete only owned parents with observed UID preconditions
   and orphan propagation. Read failure never permits deletion. Running children
   continue; recreated parents have new UIDs and may overlap old children.
5. Preserve Allow/Forbid/Replace and absent default. startingDeadlineSeconds is
   86400; both history limits are 2147483647. Conflict repair is at most three
   attempts, each reading fresh intent for spec apply. Never mutate foreign objects.
6. **Delta:** Documents ≤1 MiB, expressions ≤256 bytes, names ≤64 bytes with the shared
   run-fact alphabet. Maximum entries defaults to 8, configured 1–256 through
   --schedule-max-entries / schedules.maxEntries. There is no minimum interval
   or frequency refusal. Native cron and time zones decide when work fires.
7. Reasons/remedies meet 40/80/120; full native messages remain in Detail. After
   restart a refused parent can show paused without its reason until startup sync.
   Next time is advisory; no missing-fire timer or retry authority is added.
8. **Delta:** A schedule may use `{{...}}` only inside `templates`; every other
   spec field is literal. Check spec fields at schedule preparation on the sealed
   inspect view, before the parent is published; name the offending field and the
   remedy to move the expression into a template. A selected ref containing `{{`
   refuses with the remedy to rename the branch. Ordinary pushed runs have
   neither restriction and keep literal refs, including refs containing `{{`.

Core planning range: 1,350–1,500 Go lines; main measures 1,402 after follow-up.
The first design built came out 1.8 times its paper estimate.

## Inspections

| Requirement | Inspection |
| --- | --- |
| [REQ-C10-001](REQUIREMENTS.md#req-c10-001) | `push` (walk_step) |

The push walk checks parent and child instance labels beside a cluster-wide
default controller; only the installation's controller may act on them.
TestCronSyncGeneratedObjectGoldens
checks sealed copies and metadata; TestCronAuthoritySuspendsBeforePublication
blocks native suspension and proves the publisher is not called early.
TestCronPauseAndChildBinding and child controls check intent, replacement UID,
frozen facts and orphan recovery. TestCronFailuresRemainPending checks mixed
causes, unchanged last spec and no unsafe deletion. Forced-suspension tests make
intent/catalogue unreadable; snapshot tests reject catalogue reads; SetPause
counts one intent read. Reentry and unreadable-relevance controls check deadlines.
The schedule walk fires a non-source parent, records one child, restarts/adopts
that same UID and fires a source parent whose child loads its commit. Contract suites run against
in-memory and native Git/object adapters; unit tests alone claim no live firing.

Creating a Workflow in the pipeline namespace is equivalent to firing a schedule
(cluster administrators, Argo's own accounts and Server/UI users granted Workflow
create can do this; step accounts cannot). After a successful parent apply,
Oberth retains its published admission and its immediate published predecessor
per repository and entry, drops snapshots from failed applies, and deletes all
rows when the entry is removed. Children naming older digests are refused;
a missing live-current admission after restore is retried until visible.
Retained admissions awaiting publication can also be selected by children.
