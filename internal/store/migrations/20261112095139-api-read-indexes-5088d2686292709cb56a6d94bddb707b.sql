-- Purpose: Cover run counts and active successor checks, page terminal runs by
-- end time, and locate the latest typed audit action without scanning history.
-- Rollback: Restore a pre-upgrade backup before running an older binary.
CREATE INDEX runs_counts_idx ON runs(repo_id, status, started_at);
CREATE INDEX runs_active_ref_idx
    ON runs(repo_id, ref_kind, ref, schedule_entry, queue_sequence)
    WHERE status IN ('queued','running') AND trigger != 'promotion';
CREATE INDEX runs_terminal_end_idx
    ON runs(COALESCE(finished_at, started_at, queued_at) DESC, id DESC)
    WHERE status NOT IN ('queued','running');
CREATE INDEX audit_actions_type_idx ON audit_actions(action, resource_type, id DESC);
