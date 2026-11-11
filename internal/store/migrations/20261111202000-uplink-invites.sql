-- Purpose: Store bounded single-use CSR enrollment invitations without plaintext credentials.
-- Rollback: Restore the pre-upgrade database backup.
CREATE TABLE uplink_invites (
 id TEXT PRIMARY KEY,
 secret_digest TEXT NOT NULL UNIQUE,
 identity TEXT NOT NULL,
 server_url TEXT NOT NULL,
 ca_pin TEXT NOT NULL,
 scope_json TEXT NOT NULL,
 issuer TEXT NOT NULL,
 expires_at INTEGER NOT NULL,
 created_at INTEGER NOT NULL,
 redeemed_at INTEGER,
 CHECK(expires_at>created_at)
) WITHOUT ROWID;
