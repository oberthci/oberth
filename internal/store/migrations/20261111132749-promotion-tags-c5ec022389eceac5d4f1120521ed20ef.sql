-- Purpose: promote tag_after (#1524): an annotated tag pushed on the promoted result once the upstream target equals it.
-- Rollback: Restore the pre-upgrade database backup.
-- promotion_tags: one request per promotion. attempts and retry_after follow
-- the publication retry policy; object_sha is the annotated tag object pushed.
CREATE TABLE IF NOT EXISTS promotion_tags (
 promotion_id TEXT PRIMARY KEY REFERENCES promotions(id),
 actor TEXT NOT NULL,
 name TEXT NOT NULL,
 message TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','pushed','failed')),
 reason TEXT NOT NULL DEFAULT '',
 object_sha TEXT NOT NULL DEFAULT '',
 attempts INTEGER NOT NULL DEFAULT 0,
 retry_after INTEGER NOT NULL DEFAULT 0,
 requested_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS promotion_tags_pending_idx ON promotion_tags(status, requested_at);
