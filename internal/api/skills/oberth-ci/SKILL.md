---
name: oberth-ci
description: Follow Oberth CI, inspect failures and live holds, resume serial fixes, and promote exact unassisted evidence.
metadata:
  version: {{server-version}}
---

Reuse consent already given for the same concrete action. Obtain missing consent before a commit, push, rerun, promotion or release.
Argo executes CI; observations never submit work. Load both [fix/resume](references/fix-resume.md) and [display](references/display.md) before following a run.
This installed-version skill owns the current event loop; Tasks and decision forms are not yet offered.

An agent push requests CI with -o oberth.run=1; otherwise it is parked and creates no run.
Before pushing, take the immediate event snapshot shown below, without after_id. Save next_after_id, then push.
After pushing, keep one background events_wait shown below open while work is in flight; if background calls are unavailable, wait only after the push.
Act on every event, deduplicate IDs, resume with next_after_id even after timed_out; on a gap or lower latest_id read current state and resume from that boundary.
run.finished: read run_get; if cancelled, show who cancelled and stop without fixing code or rerunning. Otherwise, when resumed_from is absent, read failure.retry_hint.action. rerun: run_rerun once per code change; if that rerun also asks for rerun, show the reason, then stop and tell the person.
none: read run_logs for failed_step and retry_hint.detail; if the logs show a code fault, fix the code, obtain commit/push consent and push again; otherwise show the reason, detail and log lines to the person and stop.
wait (with after), retry_publication, or an unknown hint: show the reason and hint to the person, then stop; do not take an automatic action.
run.held: read run_hold_list for live availability; inspect with run_hold_exec, push the fix, then release with run_hold_abandon. Never replay an exec after an uncertain reply.
While promoting, cancel and restart the one wait with promotion.finished added; stop waiting when no work remains.
For issue changes, observe issue.updated and refresh issue_list or issue_get.
object_sha is the accepted object; commit_sha is the tested commit. Confirm delivery with publication_list.

Before pushing, use `events_wait {"snapshot":true,"own":true}`.
After consent, push the reviewed commit with `git push oberth HEAD:refs/heads/<new-branch> -o oberth.run=1`.
Then use the returned cursor in `events_wait {"kinds":["run.held","run.finished"],"own":true,"after_id":0,"timeout_s":600}`; replace 0 with that cursor.
For a finished event, use `run_get {"id":"11111111"}` with the returned exact run ID.
Failure inspection uses `run_logs {"id":"11111111","step":"test"}` with the reported failed step.
A rerun hint permits `run_rerun {"id":"11111111"}` once per code change; never repeat it without a new explicit decision.
For live holds, use `run_hold_list {"id":"11111111"}` before offering inspection.
Review the named step's currently selected hold, argv and timeout. Consent does not bind a previously displayed Pod UID.
An explicit inspected action can use `run_hold_exec {"id":"11111111","step":"test","argv":["pwd"],"timeout_s":60}`.
Commands use the step's identity and mounts and may change files or downstream outputs. Never print its environment or credential files.
After inspection and an authored fix, release with `run_hold_abandon {"id":"11111111","step":"test"}`.
Assistance remains disabled until separately enabled under the existing assist and secret-route gates.
For promotion, after exact-SHA/destination consent use `promote {"repo":"origin/team/widget","sha":"1111111111111111111111111111111111111111"}`, then observe its receipt and publication.
After a lost mutation reply or restart, read durable state before repeating an action; never replay uncertain inspection exec.
Resource reads alone do not activate this skill. Approval belongs to the originating server and this complete manifest.
For a changed manifest, unknown old URI, hash or size mismatch, stop using it and rediscover and obtain approval.
Reconnect and explicitly reload the new skill, or start a fresh conversation, before continuing CI after an upgrade.
A cached S1 event-loop skill can still name valid tools after S2 ships; valid calls cannot detect its stale workflow.
Zero discovery TTL does not remove already-loaded prose from context. Keep the server origin attached to every reference.
