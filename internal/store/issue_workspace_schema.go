package store

const issueWorkspaceMigrationVersion = 51

const issueWorkspaceSchema = `
ALTER TABLE issues ADD COLUMN work_status TEXT NOT NULL DEFAULT 'todo'
 CHECK(work_status IN ('todo', 'active', 'waiting', 'review'));
DROP TRIGGER issues_metadata_revision;
CREATE TRIGGER issues_metadata_revision
 AFTER UPDATE OF title, state, scope_upstream_id, work_status ON issues
 WHEN NEW.title IS NOT OLD.title OR NEW.state IS NOT OLD.state
   OR NEW.scope_upstream_id IS NOT OLD.scope_upstream_id OR NEW.work_status IS NOT OLD.work_status
 BEGIN UPDATE issues SET revision=revision+1 WHERE id=NEW.id; END;
`
