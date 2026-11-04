-- Purpose: Persist YAML concurrency limits and the latest accepted pipeline declarations.
-- Rollback: Restore the pre-upgrade database backup.
ALTER TABLE runs ADD COLUMN concurrency_limit INTEGER NOT NULL DEFAULT 1
 CHECK(concurrency_limit BETWEEN 1 AND 2147483647);
CREATE TABLE concurrency_group_declarations (
 repo_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 release INTEGER NOT NULL CHECK(release IN (0,1)),
 group_name TEXT NOT NULL CHECK(length(group_name) BETWEEN 1 AND 63 AND group_name NOT GLOB '*[^a-z0-9-]*'),
 group_limit INTEGER NOT NULL CHECK(group_limit BETWEEN 1 AND 2147483647),
 PRIMARY KEY(repo_id,release)
);
CREATE INDEX concurrency_group_declarations_name ON concurrency_group_declarations(group_name);
INSERT INTO concurrency_group_declarations(repo_id,release,group_name,group_limit)
 SELECT repo_id,release,concurrency_group,1 FROM (
  SELECT repo_id,release,concurrency_group,row_number() OVER (PARTITION BY repo_id,release ORDER BY queue_sequence DESC) AS n
  FROM runs WHERE concurrency_group<>''
 ) WHERE n=1;
