-- Purpose: Remove retired target node placement from retained executor configuration.
-- Rollback: Restore the verified pre-upgrade database backup.
-- Original POST intents, UIDs, admission capsules and audit evidence stay immutable.
ALTER TABLE execution_target_bindings ADD COLUMN custody_digest TEXT NOT NULL DEFAULT ''
 CHECK(custody_digest='' OR (length(custody_digest)=64 AND custody_digest NOT GLOB '*[^0-9a-f]*'));

ALTER TABLE execution_target_bindings ADD COLUMN quarantine_reason TEXT NOT NULL DEFAULT ''
 CHECK(quarantine_reason IN ('','inconsistent-placement-custody'));

CREATE TEMP TABLE removed_target_placement (
 run_id TEXT PRIMARY KEY,
 digest TEXT NOT NULL,
 last_digest TEXT NOT NULL,
 resources INTEGER NOT NULL,
 authenticated_resources INTEGER NOT NULL
);
INSERT INTO removed_target_placement
 SELECT b.run_id,
 coalesce((SELECT min(json_extract(object_json,'$.metadata.annotations."oberth.ci/execution-target"')) FROM execution_target_resources WHERE run_id=b.run_id),''),
 coalesce((SELECT max(json_extract(object_json,'$.metadata.annotations."oberth.ci/execution-target"')) FROM execution_target_resources WHERE run_id=b.run_id),''),
 (SELECT count(*) FROM execution_target_resources WHERE run_id=b.run_id),
 (SELECT count(json_extract(object_json,'$.metadata.annotations."oberth.ci/execution-target"')) FROM execution_target_resources WHERE run_id=b.run_id)
 FROM execution_target_bindings b
 WHERE json_type(binding_json,'$.Snapshot.Config.ExecutionPlacement')='object'
 AND (json_type(binding_json,'$.Snapshot.Config.ExecutionPlacement.node_name') IS NOT NULL
 OR json_type(binding_json,'$.Snapshot.Config.ExecutionPlacement.node_uid') IS NOT NULL
 OR json_type(binding_json,'$.Snapshot.Config.ExecutionPlacement.provider_id') IS NOT NULL
 OR json_type(binding_json,'$.Snapshot.Config.ExecutionPlacement.zone') IS NOT NULL
 OR json_type(binding_json,'$.Snapshot.Config.ExecutionPlacement.pool') IS NOT NULL
 OR json_type(binding_json,'$.Snapshot.Config.ExecutionPlacement.taint_key') IS NOT NULL
 OR json_type(binding_json,'$.Snapshot.Config.ExecutionPlacement.taint_value') IS NOT NULL);

-- Inconsistent custody stays immutable evidence, never authority. It must not
-- prevent unrelated runs or the server from upgrading.
UPDATE execution_target_bindings SET quarantine_reason='inconsistent-placement-custody'
 WHERE run_id IN (SELECT run_id FROM removed_target_placement
 WHERE NOT(resources=0 OR (resources=authenticated_resources AND digest=last_digest
 AND length(digest)=64 AND digest NOT GLOB '*[^0-9a-f]*')));

DROP TRIGGER execution_target_binding_immutable;
UPDATE execution_target_bindings
 SET custody_digest=(SELECT digest FROM removed_target_placement WHERE run_id=execution_target_bindings.run_id),
 binding_json=json_remove(binding_json,
 '$.Snapshot.Config.ExecutionPlacement.node_name',
 '$.Snapshot.Config.ExecutionPlacement.node_uid',
 '$.Snapshot.Config.ExecutionPlacement.provider_id',
 '$.Snapshot.Config.ExecutionPlacement.zone',
 '$.Snapshot.Config.ExecutionPlacement.pool',
 '$.Snapshot.Config.ExecutionPlacement.taint_key',
 '$.Snapshot.Config.ExecutionPlacement.taint_value')
 WHERE quarantine_reason='' AND run_id IN (SELECT run_id FROM removed_target_placement);
CREATE TRIGGER execution_target_binding_immutable BEFORE UPDATE OF run_id,workflow_name,binding_json ON execution_target_bindings BEGIN
 SELECT RAISE(ABORT,'immutable execution target binding');
END;
CREATE TRIGGER execution_target_custody_digest_immutable BEFORE UPDATE OF custody_digest ON execution_target_bindings
 WHEN OLD.custody_digest!=NEW.custody_digest BEGIN
 SELECT RAISE(ABORT,'immutable execution target custody digest');
END;
CREATE TRIGGER execution_target_quarantine_immutable BEFORE UPDATE OF quarantine_reason ON execution_target_bindings
 WHEN OLD.quarantine_reason!=NEW.quarantine_reason BEGIN
 SELECT RAISE(ABORT,'immutable execution target quarantine');
END;
DROP TABLE removed_target_placement;
