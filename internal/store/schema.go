package store

const Schema = 7

// Open and archive verification compare the complete baseline, including indexes.
const baseline = `
CREATE TABLE protocol(
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  schema_number INTEGER NOT NULL,
  identity TEXT NOT NULL,
  domain TEXT NOT NULL
);
CREATE TRIGGER protocol_immutable BEFORE UPDATE ON protocol BEGIN SELECT RAISE(ABORT,'immutable protocol');
END;
CREATE TRIGGER protocol_retained BEFORE DELETE ON protocol BEGIN SELECT RAISE(ABORT,'retained protocol'); END;
CREATE TABLE audit(
  id INTEGER PRIMARY KEY,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  resource_type TEXT NOT NULL,
  resource_id TEXT NOT NULL,
  details TEXT NOT NULL,
  created_at TEXT NOT NULL,
  previous TEXT NOT NULL,
  hash TEXT NOT NULL
);
CREATE TRIGGER audit_immutable BEFORE UPDATE ON audit BEGIN SELECT RAISE(ABORT,'immutable audit'); END;
CREATE TRIGGER audit_retained BEFORE DELETE ON audit BEGIN SELECT RAISE(ABORT,'retained audit'); END;
CREATE TABLE audit_head(
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  id INTEGER NOT NULL,
  hash TEXT NOT NULL
);
CREATE TABLE upstreams(
  name TEXT PRIMARY KEY,
  org TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL,
  base_url TEXT NOT NULL,
  key_name TEXT NOT NULL,
  removed INTEGER NOT NULL DEFAULT 0
);
CREATE TRIGGER upstream_domain BEFORE INSERT ON upstreams
WHEN EXISTS(SELECT 1 FROM upstreams WHERE name=NEW.org OR org=NEW.name)
BEGIN SELECT RAISE(ABORT,'upstream trust domain collision'); END;
CREATE TABLE repositories(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  upstream TEXT NOT NULL REFERENCES upstreams(name),
  name TEXT NOT NULL,
  canonical TEXT NOT NULL UNIQUE,
  archived INTEGER NOT NULL DEFAULT 0,
  UNIQUE(upstream,name)
);
CREATE TABLE keys(
  fingerprint TEXT PRIMARY KEY,
  identity TEXT NOT NULL,
  admin INTEGER NOT NULL,
  scope TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('operator','installer','agent')),
  park_on_push INTEGER NOT NULL,
  revoked INTEGER NOT NULL DEFAULT 0,
  revoked_at TEXT,
  expires_at TEXT,
  CHECK(park_on_push=(kind='agent'))
);
CREATE TABLE credentials(
  digest TEXT PRIMARY KEY,
  fingerprint TEXT NOT NULL REFERENCES keys(fingerprint),
  expires TEXT NOT NULL,
  revoked INTEGER NOT NULL DEFAULT 0,
  last_used TEXT
);
CREATE TABLE pins(
  spki TEXT PRIMARY KEY,
  fingerprint TEXT NOT NULL REFERENCES keys(fingerprint)
);
CREATE TABLE install_admin(
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 fingerprint TEXT NOT NULL REFERENCES keys(fingerprint),
 spki TEXT NOT NULL
);
CREATE TABLE invitations(
  id TEXT PRIMARY KEY,
  digest TEXT NOT NULL UNIQUE,
  value TEXT NOT NULL,
  consumed INTEGER NOT NULL DEFAULT 0,
  reservation TEXT NOT NULL DEFAULT '',
  reserved_until TEXT NOT NULL DEFAULT ''
);
CREATE TABLE runs(
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  repo_id INTEGER NOT NULL REFERENCES repositories(id),
  ref TEXT NOT NULL,
  status TEXT NOT NULL,
  release INTEGER NOT NULL,
  origin TEXT NOT NULL,
  summary TEXT NOT NULL,
  admission BLOB,
  findings TEXT NOT NULL DEFAULT '[]',
  revision INTEGER NOT NULL DEFAULT 1,
  evidence_complete INTEGER NOT NULL DEFAULT 0,
  evidence_sequence INTEGER NOT NULL DEFAULT 0,
  evidence_expected INTEGER NOT NULL DEFAULT 0,
  evidence_received INTEGER NOT NULL DEFAULT 0,
  step_count INTEGER NOT NULL,
  steps_planned_exact INTEGER NOT NULL,
  steps_pending INTEGER NOT NULL,
  steps_queued INTEGER NOT NULL DEFAULT 0,
  steps_running INTEGER NOT NULL DEFAULT 0,
  steps_passed INTEGER NOT NULL DEFAULT 0,
  steps_failed INTEGER NOT NULL DEFAULT 0,
  steps_skipped INTEGER NOT NULL DEFAULT 0,
  steps_unknown INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX workflow_identity ON runs(json_extract(origin,'$.workflow_uid')) WHERE json_extract(origin,
'$.workflow_uid')<>'';
CREATE INDEX run_history ON runs(repo_id,sequence DESC);
CREATE INDEX run_status ON runs(status,sequence DESC);
CREATE INDEX run_repo_status ON runs(repo_id,status,sequence DESC);
CREATE INDEX run_commit ON runs(json_extract(summary,'$.commit_sha'),repo_id,sequence DESC);
CREATE INDEX run_object ON runs(json_extract(summary,'$.object_sha'),repo_id,sequence DESC);
CREATE INDEX run_actor ON runs(json_extract(origin,'$.actor'),sequence DESC);
CREATE INDEX run_trigger ON runs(json_extract(origin,'$.trigger'),sequence DESC);
CREATE INDEX run_ref ON runs(repo_id,ref,sequence DESC);
CREATE INDEX run_unbound ON runs(sequence DESC) WHERE status IN ('queued','running') AND admission IS NOT NULL
AND json_extract(origin,'$.workflow_uid')='';
CREATE TABLE receives(
 id TEXT PRIMARY KEY,
 repo_id INTEGER NOT NULL REFERENCES repositories(id),
 binding BLOB NOT NULL
);
CREATE TABLE receive_runs(
 receive_id TEXT NOT NULL REFERENCES receives(id),
 position INTEGER NOT NULL,
 run_id TEXT NOT NULL UNIQUE REFERENCES runs(id),
 PRIMARY KEY(receive_id,position)
);
CREATE TABLE receive_heads(
 repo_id INTEGER NOT NULL REFERENCES repositories(id),
 ref TEXT NOT NULL,
 object_sha TEXT NOT NULL,
 previous_sha TEXT NOT NULL,
 PRIMARY KEY(repo_id,ref)
);
CREATE TABLE obligations(
  run_id TEXT NOT NULL REFERENCES runs(id),
  kind TEXT NOT NULL,
  revision INTEGER NOT NULL,
  value TEXT NOT NULL,
  actor TEXT NOT NULL,
  PRIMARY KEY(run_id,kind)
);
CREATE TABLE nodes(
  run_id TEXT NOT NULL REFERENCES runs(id),
  node_id TEXT NOT NULL,
  path TEXT NOT NULL,
  credential INTEGER NOT NULL,
  value TEXT NOT NULL,
  sequence INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(run_id,node_id)
);
CREATE INDEX node_path ON nodes(run_id,path);
CREATE TABLE native_steps(
 run_id TEXT PRIMARY KEY REFERENCES runs(id),
 value TEXT NOT NULL
) STRICT;
CREATE INDEX obligation_active ON obligations(run_id) WHERE value<>'';
CREATE TABLE publications(
  id TEXT PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repositories(id),
  run_id TEXT REFERENCES runs(id),
  status TEXT NOT NULL,
  value TEXT NOT NULL,
  UNIQUE(run_id,id)
);
CREATE INDEX publication_status ON publications(status,run_id);
CREATE UNIQUE INDEX released_tag ON publications(repo_id,json_extract(value,'$.release.tag'))
WHERE status='delivered' AND json_extract(value,'$.release') IS NOT NULL;
CREATE TABLE promotions(
 id TEXT PRIMARY KEY,
 repo_id INTEGER NOT NULL REFERENCES repositories(id),
 run_id TEXT REFERENCES runs(id),
 revision INTEGER NOT NULL,
 status TEXT NOT NULL,
 caller TEXT NOT NULL,
 value TEXT NOT NULL
);
-- A fresh child has one promotion; passing source evidence may be reused.
CREATE UNIQUE INDEX promotion_child_run ON promotions(run_id)
WHERE run_id IS NOT NULL AND run_id<>json_extract(value,'$.source_run_id');
CREATE TABLE issues(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  revision INTEGER NOT NULL,
  upstream TEXT NOT NULL,
  repo TEXT NOT NULL,
  state TEXT NOT NULL,
  value TEXT NOT NULL
);
CREATE INDEX issue_scope ON issues(upstream,state,id DESC);
CREATE TABLE secret_grants(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repo_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  file TEXT NOT NULL, step TEXT NOT NULL, path TEXT NOT NULL, audience TEXT NOT NULL,
  revoked INTEGER NOT NULL,
  UNIQUE(repo_id,file,step,path,audience)
);
CREATE TABLE capabilities(
  repo_id INTEGER PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
  value TEXT NOT NULL, revoked INTEGER NOT NULL
);
CREATE TABLE authority_revision(
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 revision INTEGER NOT NULL
);
CREATE TABLE authority_grants(
 kind TEXT NOT NULL,
 repo_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 file TEXT NOT NULL,
 step TEXT NOT NULL,
 path TEXT NOT NULL,
 tier TEXT NOT NULL,
 value TEXT NOT NULL,
 PRIMARY KEY(kind,repo_id,file,step,path,tier)
);
CREATE TABLE schedule_pauses(
  repo_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  entry TEXT NOT NULL, paused INTEGER NOT NULL,
  PRIMARY KEY(repo_id,entry)
);
CREATE TABLE child_proofs(
  run_id TEXT PRIMARY KEY REFERENCES runs(id), value TEXT NOT NULL
);
-- Admission ranks are 0 before publication, 1 for the predecessor and 2 for current.
CREATE TABLE schedule_admissions(
  rank INTEGER NOT NULL DEFAULT 0,
  repo_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE, entry TEXT NOT NULL,
  digest TEXT PRIMARY KEY, value BLOB NOT NULL
);
CREATE INDEX schedule_admission_entry ON schedule_admissions(repo_id,entry,rank DESC);
CREATE TABLE pvc_owners(
  claim TEXT PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repositories(id), tier TEXT NOT NULL
);
CREATE TABLE events(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repo TEXT NOT NULL,
  kind TEXT NOT NULL,
  dedupe TEXT NOT NULL UNIQUE,
  at TEXT NOT NULL,
  value TEXT NOT NULL
);
CREATE INDEX event_scope ON events(repo,id);
CREATE INDEX event_run ON events(json_extract(value,'$.run_id'),id);
CREATE INDEX event_retention ON events(kind,at,id);
CREATE INDEX event_kind_id ON events(kind,id);
CREATE INDEX event_ordinary_age ON events(at,id) WHERE kind<>'run.updated';
CREATE TABLE event_pruned(
 repo TEXT NOT NULL,
 kind TEXT NOT NULL,
 through INTEGER NOT NULL,
 PRIMARY KEY(repo,kind)
);
CREATE INDEX event_pruned_head ON event_pruned(through);
CREATE TABLE ci_projection(
  repo TEXT NOT NULL,
  ref TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  run_id TEXT NOT NULL REFERENCES runs(id),
  PRIMARY KEY(repo,ref)
);
CREATE TABLE issue_work_receipts(
  run_id TEXT PRIMARY KEY REFERENCES runs(id),
  issue_id INTEGER REFERENCES issues(id),
  sequence INTEGER NOT NULL,
  verdict TEXT NOT NULL
);
CREATE TABLE purge(
  run_id TEXT PRIMARY KEY REFERENCES runs(id),
  receipt TEXT NOT NULL,
  completed INTEGER NOT NULL DEFAULT 0
);
`
