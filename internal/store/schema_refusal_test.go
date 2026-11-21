package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestMissingProtocolRowRequiresFreshDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Create(t.Context(), path, testOptions())
	requireOK(t, err)
	var trigger string
	requireOK(t, s.writer.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name='protocol_retained'`).Scan(&trigger))
	execute(t, s.writer, `DROP TRIGGER protocol_retained`)
	execute(t, s.writer, `DELETE FROM protocol`)
	execute(t, s.writer, trigger)
	requireOK(t, s.Close())
	_, err = Open(t.Context(), path, testOptions())
	requireCode(t, err, publicerr.SchemaRefused)
	refusal := publicerr.Classify(err)
	if !errors.Is(err, sql.ErrNoRows) || refusal.Reason() != "This version requires a fresh database." || refusal.Remedy() != "Create a fresh database for this version; no upgrade path is supported." {
		t.Fatal("missing database protocol offered record lookup", err)
	}
}

func TestForeignInstallationRefusalPreservesRecoveryCause(t *testing.T) {
	for _, field := range []string{"identity", "domain"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			options := testOptions()
			if field == "identity" {
				options.Protocol.SchemaIdentity += "-other"
			} else {
				options.Protocol.AuditDomain += "-other"
			}
			s, err := Create(t.Context(), path, options)
			requireOK(t, err)
			requireOK(t, s.Close())
			_, err = Open(t.Context(), path, testOptions())
			requireCode(t, err, publicerr.SchemaRefused)
			refusal := publicerr.Classify(err)
			var mismatch *ProtocolMismatchError
			if !errors.As(err, &mismatch) || refusal.Reason() != "Database belongs to another installation." || refusal.Remedy() != "Use the installation this database belongs to." {
				t.Fatal("foreign installation lost its refusal or recovery cause", err)
			}
			s, err = Open(t.Context(), path, options)
			requireOK(t, err)
			requireOK(t, s.Close())
		})
	}
}

func TestSchemaNumberRefusalDistinguishesBinaryRollback(t *testing.T) {
	for _, tc := range []struct {
		name, reason, remedy string
		number               int
	}{
		{"older", "This version requires a fresh database.", "Create a fresh database for this version; no upgrade path is supported.", Schema - 1},
		{"newer", "Database schema is newer than this executable.", "Use the executable that supports this database version.", Schema + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			s, err := Create(t.Context(), path, testOptions())
			requireOK(t, err)
			var trigger string
			requireOK(t, s.writer.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name='protocol_immutable'`).Scan(&trigger))
			execute(t, s.writer, `DROP TRIGGER protocol_immutable`)
			_, err = s.writer.ExecContext(t.Context(), `UPDATE protocol SET schema_number=?`, tc.number)
			requireOK(t, err)
			execute(t, s.writer, trigger)
			requireOK(t, s.Close())
			_, err = Open(t.Context(), path, testOptions())
			requireCode(t, err, publicerr.SchemaRefused)
			refusal := publicerr.Classify(err)
			if refusal.Reason() != tc.reason || refusal.Remedy() != tc.remedy {
				t.Fatal("schema version has the wrong recovery advice", err)
			}
		})
	}
}

func TestDatabaseInspectionReadErrorsKeepAccessRefusal(t *testing.T) {
	s := testStore(t)
	closed := pool(s.path, "ro", testOptions(), 1)
	requireOK(t, closed.Close())
	for _, tc := range []struct {
		name  string
		check func(context.Context, *sql.DB) error
	}{
		{"protocol", func(ctx context.Context, db *sql.DB) error { return checkProtocol(ctx, db, backupOptions(s)) }},
		{"schema", checkSchema},
		{"objects", func(ctx context.Context, db *sql.DB) error { _, err := schemaObjects(ctx, db); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			canceled := tc.check(ctx, s.readers)
			if !errors.Is(canceled, context.Canceled) {
				t.Fatal("read failure lost cancellation", canceled)
			}
			for _, err := range []error{canceled, tc.check(t.Context(), closed)} {
				requireCode(t, err, publicerr.Unavailable)
				refusal := publicerr.Classify(err)
				if refusal.Reason() != "database access failed" || refusal.Remedy() != "Check the database path and permissions, then retry." {
					t.Fatal("read failure offered database replacement", err)
				}
			}
		})
	}
}
