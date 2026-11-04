-- Purpose: Persist the administrator admission hold and its absolute expiry.
-- Rollback: Restore the pre-upgrade database backup.
CREATE TABLE IF NOT EXISTS admission_holds (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 on_hold INTEGER NOT NULL CHECK(on_hold IN (0,1)),
 reason TEXT NOT NULL,
 actor TEXT NOT NULL,
 allow_tag INTEGER NOT NULL CHECK(allow_tag IN (0,1)),
 allow_release INTEGER NOT NULL CHECK(allow_release IN (0,1)),
 allow_promotion INTEGER NOT NULL CHECK(allow_promotion IN (0,1)),
 expires_at INTEGER NOT NULL,
 changed_at INTEGER NOT NULL
);
INSERT OR IGNORE INTO admission_holds VALUES(1,0,'','',1,1,0,0,0);
