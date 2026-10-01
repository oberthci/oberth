package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// ProtocolConfig is administrator-owned immutable deployment identity. It is
// never inferred from database rows: a different identity or hash domain must
// fail verification instead of silently adopting attacker-controlled metadata.
// Existing deployments must supply their original values when upgrading.
type ProtocolConfig struct {
	SchemaIdentity string
	AuditDomain    string
}

// DefaultProtocolConfig returns the protocol identifiers for a fresh deployment.
func DefaultProtocolConfig() ProtocolConfig {
	return ProtocolConfig{SchemaIdentity: oberthSchemaIdentity, AuditDomain: "oberth-audit-v1"}
}

var protocolToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (p ProtocolConfig) normalized() (ProtocolConfig, error) {
	if p == (ProtocolConfig{}) {
		p = DefaultProtocolConfig()
	}
	if err := p.Validate(); err != nil {
		return ProtocolConfig{}, err
	}
	return p, nil
}

// Validate checks an explicit configuration without opening a database.
func (p ProtocolConfig) Validate() error {
	if !protocolToken.MatchString(p.SchemaIdentity) || !protocolToken.MatchString(p.AuditDomain) {
		return fmt.Errorf("%w: protocol schema identity and audit domain must be nonempty bounded tokens", ErrInvalid)
	}
	return nil
}

// migrationSQL preserves the exact historical migration bytes for the supplied
// deployment identity. Existing ledgers and schema rows are never rewritten.
// Only the two quoted identity operands in the bootstrap SQL are parameters.
func (s *Store) migrationSQL(item migration) string {
	if item.version != 1 {
		return item.sql
	}
	return strings.ReplaceAll(item.sql, "'"+oberthSchemaIdentity+"'", "'"+s.protocol.SchemaIdentity+"'")
}

// verifyConfiguredProtocol rejects mismatched deployment parameters before any
// schema migration or recovery operation can append data under a different
// domain. The predecessor v1 has no stored hashes; its existing external-head
// migration authorization remains the only permitted migration path.
func (s *Store) verifyConfiguredProtocol(ctx context.Context) error {
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='schema_migrations'`).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	// Validate the ledger before interpreting its schema or audit records, so
	// unknown future formats and malformed migration chains retain precedence.
	version, err := s.compatibleSchemaVersion(ctx)
	if err != nil {
		return err
	}
	if version >= 2 {
		if _, err := s.verifyAuditChain(ctx, s.db); err != nil {
			return fmt.Errorf("verify configured audit protocol: %w", err)
		}
	}
	return nil
}
