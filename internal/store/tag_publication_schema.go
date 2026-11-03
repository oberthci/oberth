package store

const tagPublicationMigrationVersion = 40

// Preserve any legacy duplicate rows as evidence while preventing new ones.
const tagPublicationSchema = `
CREATE INDEX IF NOT EXISTS publications_tag_tuple_idx ON publications(repo_id,ref,lower(result_sha)) WHERE ref_kind='tag';
CREATE TRIGGER IF NOT EXISTS publications_tag_tuple_insert BEFORE INSERT ON publications
WHEN NEW.ref_kind='tag' AND EXISTS(SELECT 1 FROM publications WHERE repo_id=NEW.repo_id AND ref_kind='tag' AND ref=NEW.ref AND lower(result_sha)=lower(NEW.result_sha))
BEGIN SELECT RAISE(ABORT,'tag publication tuple already exists'); END;
`
