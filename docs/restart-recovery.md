# Run and promotion recovery

Runs waiting for admission survive an owner restart under the same run ID,
queue and admission sequences, queued time, and priority class. This applies
to branch, tag, release, and promotion runs. Recovery releases the abandoned
worker's claim so the new scheduler can admit the original run; a promotion
keeps its pending record and original CI run. A deterministic Workflow name
recorded before admission does not mean a Workflow was submitted. These runs
need no Workflow deletion. Recovery still excludes durable execution and
publication evidence from this path.

An owner restart adopts a live branch or tag Workflow only after the engine
verifies its existing run, specification, identity, and credential bindings.
Release tags use the same adoption checks.

If no resumable Workflow remains and a run has neither step results nor a
publication, recovery queues the original run ID with its original queue and
admission sequences. Its prior Workflow must be deleted through the durable
cancellation ledger before the run can be claimed again. The new attempt
recreates only that run's scratch directory and appends to its existing log.
First-observed timing milestones and admission episodes remain; subsequent
Workflow and step timing records are stored separately from prior attempts.

A pending publication is recovered from its durable intent. A tag publication
is keyed by repository, tag, and result SHA: replay reuses the original intent,
and another run cannot create a second intent for that tuple. Existing historical
publication records are retained during upgrade.

Infrastructure-only promotion interruptions retry automatically after Workflow
cleanup and publication recovery. The original actor must still have current
scope and authority, and a granted promotion must use its original, still-valid
grant. Revocation, expiry, failed CI steps, manual cancellation, and already
published work prevent automatic retry. At most two successive automatic
promotion retries are admitted by the service.

Schema versions 39 and 40 add restart timing records and tag publication guards.
They preserve existing run IDs, logs, audit records, timing and publication
history. Use the normal forward upgrade process; do not downgrade a database
that has applied these migrations.
