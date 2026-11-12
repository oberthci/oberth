package store

// Frozen shipped migration inputs; no runtime policy reads these tables.
const queuePriorityMigrationVersion = 34
const queuePrioritySchema = `
CREATE TABLE IF NOT EXISTS queue_priority_patterns (
 pattern TEXT PRIMARY KEY CHECK(length(pattern) BETWEEN 1 AND 256)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS run_priority_overrides (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 priority TEXT NOT NULL CHECK(priority IN ('high','highest')),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 512),
 actor TEXT NOT NULL,
 updated_at INTEGER NOT NULL
) WITHOUT ROWID;`

// These correlated expressions also work in RETURNING and in run-list CTEs.
// Priority tables do not shadow id/ref; run-list CTEs alias their rows as runs.
// Apply/plan cancellation guards protect external state, not queue urgency: those
// triggers remain ordinary work unless an admin pattern/bump or aging raises them.
// Only the server's tag-ref classification can get automatic release urgency.
// A branch cannot gain it through its name, release flag or trigger spelling.
// Only the oldest waiting tag per repository gets the automatic boost. Claimed
// runs still waiting for weight count too; an explicit revoke disables the boost.
// Size eligibility is checked after exact checkout, at the weighted gate.
const priorityTagReleaseSQL = `(ref_kind='tag' AND (release=1 OR trigger IN ('tag','release')))`
const tagPriorityEnabledSQL = `NOT EXISTS (SELECT 1 FROM size_grants AS priority_grant WHERE priority_grant.repo_id=runs.repo_id AND priority_grant.revoked_at IS NOT NULL)`

// v30 fills every durable admission_sequence. Use its existing status/order
// index to search waiting work, rather than walking a repository's log history.
const oldestWaitingTagSQL = `NOT EXISTS (
 SELECT 1 FROM runs AS earlier_tag INDEXED BY runs_admission_idx
 WHERE earlier_tag.status IN ('queued','running') AND earlier_tag.started_at IS NULL
 AND earlier_tag.repo_id=runs.repo_id AND earlier_tag.ref_kind='tag'
 AND (earlier_tag.release=1 OR earlier_tag.trigger IN ('tag','release'))
 AND (earlier_tag.admission_sequence,earlier_tag.queue_sequence) <
     (COALESCE(runs.admission_sequence,runs.queue_sequence),runs.queue_sequence)
)`
const automaticTagPrioritySQL = `(` + priorityTagReleaseSQL + ` AND ` + tagPriorityEnabledSQL + ` AND ` + oldestWaitingTagSQL + `)`
const naturalHighestSQL = `(trigger='promotion' OR ` + automaticTagPrioritySQL + `)`
const overridePrioritySQL = `(SELECT priority FROM run_priority_overrides WHERE run_id=id)`
const rcPatternSQL = `(SELECT pattern FROM queue_priority_patterns WHERE ref_kind='branch' AND ref GLOB pattern ORDER BY pattern LIMIT 1)`

// A submission request may only raise the automatic class. Refused and normal
// requests remain audit metadata; they never replace classification or its reason.
const legacyBasePrioritySQL = `CASE WHEN ` + naturalHighestSQL + ` THEN 'highest' WHEN ` + overridePrioritySQL + ` IS NOT NULL THEN ` + overridePrioritySQL + ` WHEN ` + requestedPrioritySQL + ` IS NOT NULL THEN ` + requestedPrioritySQL + ` WHEN ` + rcPatternSQL + ` IS NOT NULL THEN 'high' ELSE 'normal' END`
const legacyPriorityReasonSQL = `CASE WHEN trigger='promotion' THEN 'promotion run' WHEN ` + automaticTagPrioritySQL + ` THEN '` + "tag/release run: oldest waiting server-classified tag ref" + `' WHEN ` + overridePrioritySQL + ` IS NOT NULL THEN 'admin bump: ' || (SELECT reason FROM run_priority_overrides WHERE run_id=id) WHEN ` + requestedPrioritySQL + ` IS NOT NULL THEN 'submission priority: ' || (SELECT reason FROM run_priority_requests WHERE run_id=id) WHEN ` + rcPatternSQL + ` IS NOT NULL THEN 'release-candidate pattern: ' || ` + rcPatternSQL + ` WHEN ` + priorityTagReleaseSQL + ` THEN CASE WHEN NOT ` + tagPriorityEnabledSQL + ` THEN 'tag/release automatic priority disabled by size_revoke' ELSE 'tag/release waiting behind an earlier tag of this repository' END ELSE 'ordinary run' END`

const prioritySubmissionMigrationVersion = 39
const prioritySubmissionSchema = `
CREATE TABLE IF NOT EXISTS priority_grants (
 identity TEXT PRIMARY KEY,
 max_class TEXT NOT NULL CHECK(max_class IN ('high','highest')),
 expires INTEGER NOT NULL,
 granted_by TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS run_priority_requests (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 requested TEXT NOT NULL,
 accepted TEXT NOT NULL CHECK(accepted IN ('normal','high','highest')),
 reason TEXT NOT NULL,
 actor TEXT NOT NULL,
 refusal TEXT NOT NULL
) WITHOUT ROWID;
INSERT OR IGNORE INTO run_priority_requests(run_id,requested,accepted,reason,actor,refusal)
 SELECT id,'',` + legacyBasePrioritySQL + `,` + legacyPriorityReasonSQL + `,actor,'' FROM runs
 WHERE status IN ('queued','running') AND (` + legacyBasePrioritySQL + `) IN ('high','highest');`

const requestedPrioritySQL = `(SELECT accepted FROM run_priority_requests WHERE run_id=id AND requested!='' AND accepted IN ('high','highest') AND refusal='')`

// Explicit priority, including an admin bump, survives later pushes. RC runs
// retain the administrator's existing live classification.
