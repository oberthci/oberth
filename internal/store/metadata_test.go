package store

import (
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func TestStateMetadataReadsLiveSchemaAndProtocol(t *testing.T) {
	t.Parallel()
	s := metadataStore(t)
	schema, protocol, err := s.StateMetadata(t.Context())
	if err != nil || schema != Schema || protocol != stateport.DefaultProtocol() {
		t.Fatal("live metadata missing", err)
	}
	if _, err := s.writer.ExecContext(t.Context(), `UPDATE protocol SET schema_number=?,identity=?,domain=? WHERE singleton=1`, Schema+1, "next-schema", "next-audit"); err != nil {
		t.Fatal(err)
	}
	schema, protocol, err = s.StateMetadata(t.Context())
	if err != nil || schema != Schema+1 || protocol.SchemaIdentity != "next-schema" || protocol.AuditDomain != "next-audit" {
		t.Fatal("metadata came from compiled or configured facts", err)
	}
}

func TestStateMetadataRefusesAbsentOrInvalidLiveFacts(t *testing.T) {
	t.Parallel()
	for _, statement := range []string{`DELETE FROM protocol`, `UPDATE protocol SET identity=''`} {
		t.Run(statement, func(t *testing.T) {
			s := metadataStore(t)
			if _, err := s.writer.ExecContext(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
			schema, protocol, err := s.StateMetadata(t.Context())
			if err == nil || schema != 0 || protocol != (stateport.Protocol{}) {
				t.Fatal("fabricated metadata on failed read")
			}
		})
	}
}

func metadataStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	for _, query := range []string{`DROP TRIGGER protocol_immutable`, `DROP TRIGGER protocol_retained`} {
		if _, err := s.writer.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
