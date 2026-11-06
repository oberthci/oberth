-- Purpose: Bind exact placed parent Workflow intent and original API UID.
-- Rollback: Restore the verified pre-upgrade database backup.
CREATE UNIQUE INDEX protected_placement_workflow_audit_idx ON audit_actions(resource_id)
 WHERE action='protected.placement-workflow' AND resource_type='workflow';
CREATE UNIQUE INDEX protected_placement_workflow_uid_audit_idx ON audit_actions(resource_id)
 WHERE action='protected.placement-workflow-bound' AND resource_type='workflow';
