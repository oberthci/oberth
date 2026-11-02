package store

const issueRevisionMigrationVersion = 40
const issueRevisionSchema = `
ALTER TABLE issues ADD COLUMN revision INTEGER NOT NULL DEFAULT 1
 CHECK(typeof(revision)='integer' AND revision>0);
CREATE TRIGGER issues_metadata_revision
 AFTER UPDATE OF title, state, scope_upstream_id ON issues
 WHEN NEW.title IS NOT OLD.title OR NEW.state IS NOT OLD.state OR NEW.scope_upstream_id IS NOT OLD.scope_upstream_id
 BEGIN UPDATE issues SET revision=revision+1 WHERE id=NEW.id; END;
`
