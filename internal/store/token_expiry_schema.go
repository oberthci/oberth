package store

const tokenExpiryMigrationVersion = 40

// #nosec G101 -- This is SQL schema text naming a table; it contains no credential value.
const tokenExpirySchema = `ALTER TABLE token_credentials ADD COLUMN expires_at INTEGER CHECK(expires_at IS NULL OR typeof(expires_at)='integer');`
