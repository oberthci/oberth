package store

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestRecoveryChecksIntegrityAndForeignKeysAfterMemberHash(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"integrity", "foreign-key"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			atomicRepository(t, s)
			path := filepath.Join(t.TempDir(), "damaged.sqlite")
			requireOK(t, snapshotDatabase(t.Context(), s.path, path, stateport.DefaultMaxBytes))
			db, err := sql.Open("sqlite", path)
			requireOK(t, err)
			db.SetMaxOpenConns(1)
			if kind == "integrity" {
				execute(t, db, `PRAGMA ignore_check_constraints=ON`)
				execute(t, db, `INSERT INTO keys(fingerprint,identity,admin,scope,kind,park_on_push,revoked) VALUES('bad','bad',0,'[]','operator',1,0)`)
				execute(t, db, `PRAGMA ignore_check_constraints=OFF`)
			} else {
				execute(t, db, `INSERT INTO pins VALUES('orphan','absent-key')`)
			}
			requireOK(t, db.Close())
			report := stateport.BackupReport{Schema: Schema, AuditHead: independentHead(t, s.readers), Integrity: true, ForeignKeys: true}
			var archive bytes.Buffer
			requireOK(t, emitArchive(t.Context(), archiveOutput{path, backupOptions(s), report, &archive}))
			destination := filepath.Join(t.TempDir(), "restored.sqlite")
			_, err = backupForTest(t, s).Restore(t.Context(), restoreRequest(s, destination), bytes.NewReader(archive.Bytes()))
			requireCode(t, err, publicerr.SchemaRefused)
			if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("damaged database published", err)
			}
		})
	}
}

func TestRecoveryErrorsNameDistinctCauses(t *testing.T) {
	t.Parallel()
	for _, kind := range []refusal{locked, diskFull, schema, damagedArchive} {
		var public *publicerr.Error
		err := refuse(kind, nil)
		if !errors.As(err, &public) {
			t.Fatal(err)
		}
		requireOK(t, publicerr.CheckMessage(public.Reason(), 80))
		requireOK(t, publicerr.CheckMessage(public.Remedy(), 120))
		if kind == damagedArchive && strings.Contains(strings.ToLower(public.Remedy()), "restore") {
			t.Fatal("circular restore advice")
		}
	}
	err := databaseError(&os.PathError{Op: "write", Path: "private", Err: syscall.ENOSPC})
	var public *publicerr.Error
	if !errors.As(err, &public) || public.Reason() != diagnostics()[diskFull].reason || !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("disk full cause lost", err)
	}
}

func TestRestoreFullDiskNeverPublishesDestination(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var archive bytes.Buffer
	_, err := s.Snapshot(t.Context(), backupOptions(s), &archive)
	requireOK(t, err)
	b := &Backup{path: s.path, syncFile: func(string) error { return syscall.ENOSPC }}
	destination := filepath.Join(t.TempDir(), "restored.sqlite")
	_, err = b.Restore(t.Context(), restoreRequest(s, destination), bytes.NewReader(archive.Bytes()))
	var public *publicerr.Error
	if !errors.As(err, &public) || public.Reason() != diagnostics()[diskFull].reason {
		t.Fatal(err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disk full published destination", err)
	}
}
