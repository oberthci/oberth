-- Purpose: Remove Oberth resource/queue admission; materialize every accepted receive.
-- Rollback: Restore the pre-upgrade database backup before running an older binary.
-- The mapping is transaction-local. Receive IDs remain the replay authority.
CREATE TEMP TABLE native_receives AS
 SELECT d.event_id,lower(hex(randomblob(16))) AS run_id,d.run_spec,d.reason,e.created_at
 FROM deferred_branch_receives d JOIN receive_events e ON e.id=d.event_id
 WHERE e.run_id='' ORDER BY d.sequence;
INSERT INTO runs(id,repo_id,ref_kind,ref,sha,actor,release,credentialed,trigger,phase,
 tested_sha,base_sha,schedule_entry,status,reason,assist_requested,queued_at,created_at,updated_at,finished_at)
 SELECT n.run_id,e.repo_id,e.ref_kind,e.ref,e.commit_sha,e.actor,
 COALESCE(json_extract(n.run_spec,'$.Run.Release'),0),COALESCE(json_extract(n.run_spec,'$.Run.Credentialed'),0),
 json_extract(n.run_spec,'$.Run.Trigger'),CASE WHEN n.reason='ref deleted' THEN 'cancelled' ELSE 'queued' END,
 COALESCE(json_extract(n.run_spec,'$.Run.TestedSHA'),''),COALESCE(json_extract(n.run_spec,'$.Run.BaseSHA'),''),
 COALESCE(json_extract(n.run_spec,'$.Run.ScheduleEntry'),''),
 CASE WHEN n.reason='ref deleted' THEN 'interrupted' ELSE 'queued' END,
 CASE WHEN n.reason='ref deleted' THEN n.reason ELSE COALESCE(json_extract(n.run_spec,'$.Run.Reason'),'') END,
 COALESCE(json_extract(n.run_spec,'$.Run.Assist'),0),n.created_at,n.created_at,n.created_at,
 CASE WHEN n.reason='ref deleted' THEN n.created_at ELSE NULL END
 FROM native_receives n JOIN receive_events e ON e.id=n.event_id ORDER BY n.rowid;
UPDATE runs SET admission_sequence=queue_sequence WHERE id IN (SELECT run_id FROM native_receives);
-- Earlier standard branch arrivals are superseded by the latest accepted one;
-- Workflows already running at upgrade are deliberately unaffected.
UPDATE runs SET status='interrupted',phase='cancelled',reason='superseded',
 superseded_by=(SELECT newer.id FROM runs newer JOIN native_receives n ON n.run_id=newer.id
 WHERE newer.repo_id=runs.repo_id AND newer.ref_kind=runs.ref_kind AND newer.ref=runs.ref
 AND newer.status='queued' AND newer.queue_sequence>runs.queue_sequence
 ORDER BY newer.queue_sequence DESC LIMIT 1),finished_at=updated_at
 WHERE id IN (SELECT run_id FROM native_receives) AND status='queued' AND EXISTS
 (SELECT 1 FROM runs newer JOIN native_receives n ON n.run_id=newer.id
 WHERE newer.repo_id=runs.repo_id AND newer.ref_kind=runs.ref_kind AND newer.ref=runs.ref
 AND newer.status='queued' AND newer.queue_sequence>runs.queue_sequence);
UPDATE receive_events SET run_id=(SELECT run_id FROM native_receives n WHERE n.event_id=receive_events.id)
 WHERE id IN (SELECT event_id FROM native_receives);
DROP TABLE native_receives;
DROP TABLE deferred_branch_receives;
DROP TABLE queue_cap;
DROP TABLE resource_ceilings;
DROP TABLE size_grants;
DROP TABLE admission_capacity_override;
DROP TABLE concurrency_group_declarations;
