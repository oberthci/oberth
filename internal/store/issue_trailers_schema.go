package store

const issueTrailersMigrationVersion = 55

// Historical deliveries seed the green boundary without replaying old trailers.
const issueTrailersSchema = `
CREATE TABLE issue_trailer_publications (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 publication_id TEXT NOT NULL UNIQUE,
 repo_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 branch TEXT NOT NULL,
 sha TEXT NOT NULL,
 previous_sha TEXT NOT NULL DEFAULT '',
 run_id TEXT NOT NULL,
 done INTEGER NOT NULL DEFAULT 0 CHECK(done IN (0,1))
);
CREATE INDEX issue_trailer_pending_idx ON issue_trailer_publications(sequence) WHERE done=0;
CREATE INDEX issue_trailer_head_idx ON issue_trailer_publications(repo_id,branch,sequence DESC);
INSERT INTO issue_trailer_publications(publication_id,repo_id,branch,sha,run_id,done)
 SELECT p.id,p.repo_id,p.ref,p.result_sha,COALESCE(NULLIF(p.run_id,''),m.run_id,''),1
 FROM publications p
 JOIN repositories r ON r.id=p.repo_id
 LEFT JOIN runs u ON u.id=p.run_id
 LEFT JOIN promotions m ON m.id=p.promotion_id
 WHERE p.status='delivered' AND p.ref_kind='branch' AND p.ref=r.default_branch
 AND ((u.status='passed' AND u.trigger IN ('push','branch') AND u.ref=r.default_branch)
      OR m.status='passed')
 ORDER BY p.updated_at,p.rowid;
`
