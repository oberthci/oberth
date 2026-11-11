-- Purpose: Bind client SPKI pins to uplinks and distinguish certificate-only lifecycle records.
-- Rollback: Restore the pre-upgrade database backup.
ALTER TABLE token_credentials ADD COLUMN certificate_only INTEGER NOT NULL DEFAULT 0 CHECK(certificate_only IN (0,1));
CREATE TABLE uplink_client_pins (
 pin TEXT PRIMARY KEY,
 uplink_id INTEGER NOT NULL REFERENCES uplinks(id) ON DELETE CASCADE,
 expires_at INTEGER NOT NULL,
 created_at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX uplink_client_pins_uplink_idx ON uplink_client_pins(uplink_id, expires_at);
