-- Purpose: Retire extended-resource grant settings while preserving operator API authority and audit history.
-- Rollback: Restore the verified pre-upgrade database backup.
UPDATE workflow_capability_grants
 SET capability_json=json_remove(capability_json,'$.extended_resources')
 WHERE json_type(capability_json,'$.extended_resources') IS NOT NULL;
