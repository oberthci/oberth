-- Purpose: Remove Oberth resource/queue admission; materialize every accepted receive.
-- Rollback: Restore the pre-upgrade database backup before running an older binary.
-- The mapping is transaction-local. Receive IDs remain the replay authority.
CREATE TEMP TABLE native_receives AS
 SELECT d.event_id,lower(hex(randomblob(16))) AS run_id,d.run_spec,d.reason,e.created_at,
 COALESCE((SELECT a.action FROM audit_actions a
 WHERE a.resource_type='repository' AND a.resource_id=CAST(e.repo_id AS TEXT)
 AND a.action IN ('repository.archive','repository.unarchive') ORDER BY a.id DESC LIMIT 1),'')='repository.archive' AS archived
 FROM deferred_branch_receives d JOIN receive_events e ON e.id=d.event_id
 WHERE e.run_id='' ORDER BY d.sequence;
INSERT INTO runs(id,repo_id,ref_kind,ref,sha,actor,release,credentialed,trigger,phase,
 tested_sha,base_sha,schedule_entry,status,reason,assist_requested,queued_at,created_at,updated_at,finished_at)
 SELECT n.run_id,e.repo_id,e.ref_kind,e.ref,e.commit_sha,e.actor,
 COALESCE(json_extract(n.run_spec,'$.Run.Release'),0),COALESCE(json_extract(n.run_spec,'$.Run.Credentialed'),0),
 json_extract(n.run_spec,'$.Run.Trigger'),CASE WHEN n.reason='ref deleted' OR n.archived THEN 'cancelled' ELSE 'queued' END,
 COALESCE(json_extract(n.run_spec,'$.Run.TestedSHA'),''),COALESCE(json_extract(n.run_spec,'$.Run.BaseSHA'),''),
 COALESCE(json_extract(n.run_spec,'$.Run.ScheduleEntry'),''),
 CASE WHEN n.reason='ref deleted' OR n.archived THEN 'interrupted' ELSE 'queued' END,
 CASE WHEN n.reason='ref deleted' THEN n.reason WHEN n.archived THEN 'repository archived' ELSE COALESCE(json_extract(n.run_spec,'$.Run.Reason'),'') END,
 COALESCE(json_extract(n.run_spec,'$.Run.Assist'),0),n.created_at,n.created_at,n.created_at,
 CASE WHEN n.reason='ref deleted' OR n.archived THEN n.created_at ELSE NULL END
 FROM native_receives n JOIN receive_events e ON e.id=n.event_id ORDER BY n.rowid;
UPDATE runs SET admission_sequence=queue_sequence WHERE id IN (SELECT run_id FROM native_receives);
-- Match normal enqueue's immutable issue-work registration. Append after existing
-- branch/promotion work, in receipt materialization order, before removing IDs.
INSERT INTO ci_issue_work(repo_id,branch,origin,run_id,actor,created_at)
 SELECT r.repo_id,r.ref,'branch',r.id,r.actor,r.created_at
 FROM native_receives n JOIN runs r ON r.id=n.run_id
 JOIN repositories p ON p.id=r.repo_id
 WHERE r.ref_kind='branch' AND r.trigger NOT IN ('promotion','plan','apply')
 AND (r.trigger='schedule' OR r.ref=p.default_branch)
 ORDER BY n.rowid;
-- Apply runtime supersession within each branch/schedule lane, including
-- older already-queued work. Running Workflows and pending publication custody
-- are not altered by this upgrade. Every receipt still maps to its own run.
CREATE TEMP TABLE native_supersessions AS
 WITH candidates AS (
 SELECT older.id,newer.id AS successor,newer.queued_at AS finished_at,
 row_number() OVER(PARTITION BY older.id ORDER BY newer.queue_sequence DESC) AS rank
 FROM runs older JOIN runs newer ON newer.repo_id=older.repo_id
 AND newer.ref_kind=older.ref_kind AND newer.ref=older.ref
 AND newer.schedule_entry=older.schedule_entry
 JOIN native_receives n ON n.run_id=newer.id
 WHERE older.status='queued' AND newer.status='queued'
 AND older.ref_kind='branch' AND older.trigger!='promotion' AND newer.trigger!='promotion'
 AND newer.queue_sequence>older.queue_sequence
 AND NOT EXISTS(SELECT 1 FROM publications p WHERE p.run_id=older.id AND p.status='pending')
 ) SELECT id,successor,finished_at FROM candidates WHERE rank=1;
UPDATE runs SET status='interrupted',phase='interrupted',reason='superseded by newer run',
 superseded_by=(SELECT successor FROM native_supersessions n WHERE n.id=runs.id),
 finished_at=(SELECT finished_at FROM native_supersessions n WHERE n.id=runs.id),
 updated_at=(SELECT finished_at FROM native_supersessions n WHERE n.id=runs.id)
 WHERE id IN (SELECT id FROM native_supersessions);
INSERT INTO run_cancellations(run_id,job_name,superseded_by,reason,created_at,completed_at)
 SELECT r.id,r.job_name,n.successor,'superseded',n.finished_at,NULL
 FROM runs r JOIN native_supersessions n ON n.id=r.id WHERE r.job_name!=''
 ON CONFLICT(run_id) DO UPDATE SET job_name=excluded.job_name,
 superseded_by=excluded.superseded_by,reason=excluded.reason,created_at=excluded.created_at,completed_at=NULL;
DROP TABLE native_supersessions;
UPDATE receive_events SET run_id=(SELECT run_id FROM native_receives n WHERE n.event_id=receive_events.id)
 WHERE id IN (SELECT event_id FROM native_receives);
DROP TABLE native_receives;
DROP TABLE deferred_branch_receives;
DROP TABLE queue_cap;
DROP TABLE resource_ceilings;
DROP TABLE size_grants;
DROP TABLE admission_capacity_override;
DROP TABLE concurrency_group_declarations;
