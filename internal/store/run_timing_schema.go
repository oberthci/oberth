package store

// v30 is stacked after #1102's v29. Do not fill its reserved slot with a no-op.
const runTimingMigrationVersion = 30
const runTimingSchema = `
CREATE TABLE run_timings (
    run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK(version = 1),
    claimed_at INTEGER,
    checkout_ready_at INTEGER,
    source_seed_started_at INTEGER,
    source_ready_at INTEGER,
    workflow_created_at INTEGER,
    terminal_at INTEGER,
    workflow_json TEXT CHECK(workflow_json IS NULL OR length(workflow_json) <= 262144)
);
CREATE TABLE run_admission_timings (
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    episode INTEGER NOT NULL CHECK(episode > 0),
    requested_at INTEGER,
    granted_at INTEGER,
    closed_at INTEGER,
    PRIMARY KEY(run_id, episode)
);
CREATE UNIQUE INDEX run_admission_timing_open ON run_admission_timings(run_id) WHERE closed_at IS NULL;
CREATE TABLE step_timings (
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    node_id TEXT NOT NULL,
    workflow_uid TEXT NOT NULL,
    pod_uid TEXT NOT NULL,
    available INTEGER NOT NULL CHECK(available IN (0,1)),
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    timing_json TEXT NOT NULL CHECK(length(timing_json) <= 16384),
    PRIMARY KEY(run_id, node_id)
);
CREATE TRIGGER run_timing_claim AFTER UPDATE OF status ON runs
WHEN OLD.status = 'queued' AND NEW.status = 'running'
BEGIN
    INSERT OR IGNORE INTO run_timings(run_id,version) VALUES(NEW.id,1);
    UPDATE run_timings SET claimed_at = COALESCE(claimed_at,NEW.updated_at) WHERE run_id=NEW.id;
END;
CREATE TRIGGER run_timing_grant AFTER UPDATE OF started_at ON runs
WHEN OLD.started_at IS NULL AND NEW.started_at IS NOT NULL AND NEW.phase = 'waiting' AND NEW.status = 'running'
BEGIN
    INSERT OR IGNORE INTO run_timings(run_id,version) VALUES(NEW.id,1);
    INSERT INTO run_admission_timings(run_id,episode)
        SELECT NEW.id,COALESCE(MAX(episode),0)+1 FROM run_admission_timings WHERE run_id=NEW.id
        HAVING NOT EXISTS(SELECT 1 FROM run_admission_timings WHERE run_id=NEW.id AND closed_at IS NULL);
    UPDATE run_admission_timings SET granted_at=COALESCE(granted_at,NEW.started_at) WHERE run_id=NEW.id AND closed_at IS NULL;
END;
CREATE TRIGGER run_timing_park AFTER UPDATE OF started_at ON runs
WHEN OLD.started_at IS NOT NULL AND NEW.started_at IS NULL AND NEW.status = 'running'
BEGIN
    UPDATE run_admission_timings SET closed_at=NEW.updated_at WHERE run_id=NEW.id AND closed_at IS NULL;
END;
CREATE TRIGGER run_timing_terminal AFTER UPDATE OF status ON runs
WHEN OLD.finished_at IS NULL AND NEW.finished_at IS NOT NULL
BEGIN
    INSERT OR IGNORE INTO run_timings(run_id,version) VALUES(NEW.id,1);
    UPDATE run_timings SET terminal_at=COALESCE(terminal_at,NEW.finished_at) WHERE run_id=NEW.id;
    UPDATE run_admission_timings SET closed_at=NEW.finished_at WHERE run_id=NEW.id AND closed_at IS NULL;
END;
CREATE TRIGGER run_timing_immutable BEFORE UPDATE ON run_timings
WHEN NEW.run_id != OLD.run_id OR NEW.version != OLD.version
 OR (OLD.claimed_at IS NOT NULL AND NEW.claimed_at IS NOT OLD.claimed_at)
 OR (OLD.checkout_ready_at IS NOT NULL AND NEW.checkout_ready_at IS NOT OLD.checkout_ready_at)
 OR (OLD.source_seed_started_at IS NOT NULL AND NEW.source_seed_started_at IS NOT OLD.source_seed_started_at)
 OR (OLD.source_ready_at IS NOT NULL AND NEW.source_ready_at IS NOT OLD.source_ready_at)
 OR (OLD.workflow_created_at IS NOT NULL AND NEW.workflow_created_at IS NOT OLD.workflow_created_at)
 OR (OLD.terminal_at IS NOT NULL AND NEW.terminal_at IS NOT OLD.terminal_at)
 OR (OLD.workflow_json IS NOT NULL AND NEW.workflow_json IS NOT OLD.workflow_json)
BEGIN SELECT RAISE(ABORT,'run timing is immutable'); END;
CREATE TRIGGER admission_timing_immutable BEFORE UPDATE ON run_admission_timings
WHEN NEW.run_id != OLD.run_id OR NEW.episode != OLD.episode
 OR (OLD.requested_at IS NOT NULL AND NEW.requested_at IS NOT OLD.requested_at)
 OR (OLD.granted_at IS NOT NULL AND NEW.granted_at IS NOT OLD.granted_at)
 OR (OLD.closed_at IS NOT NULL AND NEW.closed_at IS NOT OLD.closed_at)
BEGIN SELECT RAISE(ABORT,'admission timing is immutable'); END;
CREATE TRIGGER step_timing_immutable BEFORE UPDATE ON step_timings
WHEN NEW.run_id != OLD.run_id OR NEW.node_id != OLD.node_id OR NEW.workflow_uid != OLD.workflow_uid
 OR (OLD.pod_uid != '' AND NEW.pod_uid != OLD.pod_uid)
 OR (OLD.available = 1 AND NEW.timing_json != OLD.timing_json)
BEGIN SELECT RAISE(ABORT,'step timing attempt is immutable'); END;
`
