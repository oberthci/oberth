package store

const tokenExpiryMigrationVersion = 39
const tokenExpirySchema = `ALTER TABLE token_credentials ADD COLUMN expires_at INTEGER CHECK(expires_at IS NULL OR typeof(expires_at)='integer');`
