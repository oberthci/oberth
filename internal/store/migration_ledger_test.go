package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMigrationDefinitionsRejectDuplicateVersions(t *testing.T) {
	t.Parallel()
	items := append([]migration(nil), migrations...)
	items = append(items, migrations[len(migrations)-1])
	err := validateMigrations(items, latestMigrationVersion)
	if !errors.Is(err, ErrSchemaIncompatible) || !strings.Contains(err.Error(), fmt.Sprintf("duplicate migration version %d", latestMigrationVersion)) {
		t.Fatalf("duplicate migration: %v", err)
	}
}

func TestOpenRejectsMissingLowerMigration(t *testing.T) {
	t.Parallel()
	for _, opener := range []struct {
		name string
		open func(context.Context, string, Options) (*Store, error)
	}{
		{"owner", Open}, {"admin", OpenAdminClient},
		{"current", OpenCurrent}, {"inspection", InspectCurrent},
	} {
		t.Run(opener.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "gap.sqlite")
			s, err := Open(ctx, path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			missing := latestMigrationVersion - 1
			if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, missing); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := opener.open(ctx, path, Options{})
			if opened != nil {
				_ = opened.Close()
			}
			want := fmt.Sprintf("missing version %d below applied version %d", missing, latestMigrationVersion)
			if !errors.Is(err, ErrSchemaIncompatible) || !strings.Contains(err.Error(), want) {
				t.Fatalf("open gap = %v, want %q", err, want)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			var count int
			if err := raw.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = ?`, missing).Scan(&count); err != nil || count != 0 {
				t.Fatalf("refused gap was backfilled: count=%d err=%v", count, err)
			}
		})
	}
}

func TestExistingV24MigrationLedgerPreservedWithoutReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "existing-v24.sqlite")
	original := time.Unix(100, 0)
	s, err := Open(ctx, path, Options{Now: func() time.Time { return original }})
	if err != nil {
		t.Fatal(err)
	}
	// Existing databases already carry a row for every applied migration. Any
	// replay would fail on their existing tables, indexes or audit bootstrap.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenCurrent(ctx, path, Options{Now: func() time.Time { return original.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for version := 1; version <= 24; version++ {
		var appliedAt int64
		if err := s.db.QueryRow(`SELECT applied_at FROM schema_migrations WHERE version = ?`, version).Scan(&appliedAt); err != nil {
			t.Fatal(err)
		}
		if appliedAt != unixNano(original) {
			t.Fatalf("migration %d was replayed: applied_at=%d", version, appliedAt)
		}
	}
	assertFreshSchemaLedger(t, s.db)
}
