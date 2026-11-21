package store

import (
	"context"

	"github.com/oberthci/oberth/internal/stateport"
)

// StateMetadata reads the live protocol row, independently of compiled schema or options.
// It supplies the upgrade floor without transferring a database snapshot.
func (s *Store) StateMetadata(ctx context.Context) (int, stateport.Protocol, error) {
	var schema int
	var protocol stateport.Protocol
	err := s.readers.QueryRowContext(ctx, `SELECT schema_number,identity,domain FROM protocol WHERE singleton=1`).Scan(&schema, &protocol.SchemaIdentity, &protocol.AuditDomain)
	if err != nil {
		return 0, stateport.Protocol{}, databaseError(err)
	}
	if schema <= 0 || protocol.Validate() != nil {
		return 0, stateport.Protocol{}, refuse(corrupt, nil)
	}
	return schema, protocol, nil
}
