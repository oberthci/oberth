package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/store/storetest"
)

func testOptions() Options {
	return Options{Protocol: stateport.DefaultProtocol(), Clock: func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }}
}

//nolint:gochecknoglobals // Each test opens a private copy of one closed database image.
var baselineDatabase = storetest.New(func(t *testing.T, path string) {
	s, err := Create(t.Context(), path, testOptions())
	requireOK(t, err)
	requireOK(t, s.Close())
})

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), baselineDatabase.Path(t), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func backupOptions(s *Store) stateport.Options {
	return stateport.Options{Protocol: s.options.Protocol, Schema: Schema}
}

func TestBackupPortContract(t *testing.T) {
	t.Parallel()
	contracttest.RunContract(t, func(t *testing.T, index int) contracttest.Fixture {
		t.Helper()
		options := testOptions()
		if index == 1 {
			options.Protocol.SchemaIdentity += "-other"
		}
		if index == 3 {
			options.Protocol.AuditDomain += "-other"
		}
		s, err := Create(t.Context(), filepath.Join(t.TempDir(), "state.sqlite"), options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		// Explicit setup is the independent content oracle, not an authority-path test.
		if _, err := s.writer.ExecContext(t.Context(), `INSERT INTO upstreams(name,org,kind,base_url,key_name) VALUES('origin','team','git','https://oberth.ci','source')`); err != nil {
			t.Fatal(err)
		}
		expected := backupOptions(s)
		foreignSchemaFixture(t, s, index, &expected)
		head := stateport.AuditHead{SHA256: genesis()}
		return contracttest.Fixture{Port: backupForTest(t, s), Options: expected, WantContent: "origin/team", WantHead: head, Read: readDestination}
	})
}

func readDestination(t *testing.T, path string) (string, stateport.AuditHead, bool) {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", stateport.AuditHead{}, false
	}
	db := pool(path, "ro", testOptions(), 1)
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var name, org string
	var head stateport.AuditHead
	if err := db.QueryRowContext(t.Context(), `SELECT name,org FROM upstreams`).Scan(&name, &org); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT id,hash FROM audit_head`).Scan(&head.ID, &head.SHA256); err != nil {
		t.Fatal(err)
	}
	return name + "/" + org, head, true
}

func execute(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), statement); err != nil {
		t.Fatal(err)
	}
}

func foreignSchemaFixture(t *testing.T, s *Store, index int, expected *stateport.Options) {
	t.Helper()
	if index == 2 {
		var trigger string
		if err := s.writer.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name='protocol_immutable'`).Scan(&trigger); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{`DROP TRIGGER protocol_immutable`, `UPDATE protocol SET schema_number=` + fmt.Sprint(Schema+1), trigger} {
			if _, err := s.writer.ExecContext(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
		}
		expected.Schema = Schema + 1
	}
}

func TestNativeSchemaVersionIsPersisted(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var version int
	if err := s.readers.QueryRowContext(t.Context(), `SELECT schema_number FROM protocol`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if Schema != 7 || version != 7 || backupOptions(s).Schema != 7 {
		t.Fatal("native progress schema is not versioned", Schema, version)
	}
}
