# Run and promotion recovery

Oberth adopts an existing Workflow after an owner restart using its original run,
specification, identity, and credential bindings. Branch, tag, release, and
promotion Workflows keep their original run ID. Shutdown leaves running
Workflows alone for the successor to adopt.

A run whose Workflow is confirmed absent, including a claimed run that never
submitted a Workflow, fails with `interrupted by server restart`. Recovery does
not resubmit it. Users can request an explicit retry or rerun through the
existing authorization checks; that creates a new run ID. Unclaimed queued runs
keep their existing queue order. Runs listed in a restored backup's marker are
cancelled with `restored from a backup; not dispatched again` instead.

An ambiguous Workflow creation remains under bounded observation. An initial
absence or an API error does not prove the Workflow was never created. Recovery
adopts a Workflow that appears, and concludes absence only after the existing
observation boundary and a successful absence read. It does not delete a live
Workflow to retry creation.

First-observed timing milestones, admission episodes, and bound Workflow and
step evidence remain durable. The forward timing migration retains the current
attempt's evidence in the ordinary timing tables and drops the retired restart
attempt tables. Historical audit records remain immutable.

A pending publication is recovered from its durable intent. A tag publication
is keyed by repository, tag, and result SHA: replay reuses the original intent,
and another run cannot create a second intent for that tuple. Existing
publication records are retained during upgrade.

Interrupted promotions are not automatically resubmitted on restart. Explicit
user retries retain their authorization checks. The separate moved-target
publication retry policy remains unchanged.

Use the normal forward upgrade process; do not downgrade a database that has
applied these migrations.
