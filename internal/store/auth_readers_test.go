package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/oberthci/oberth/internal/model"
)

func authReaderFixture(t *testing.T) (*Store, model.Uplink) {
	t.Helper()
	now := time.Now()
	s := testStore(t, &now)
	if err := s.OpenSSHAuthReaders(t.Context()); err != nil {
		t.Fatal(err)
	}
	credential, err := s.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "auth-test", Digest: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := s.UpsertUplink(t.Context(), model.UplinkSpec{Fingerprint: "SHA256:auth-test", Identity: "auth-test", TokenCredentialID: credential.ID, AuthActor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(t.Context(), credential.ID); err != nil {
		t.Fatal(err)
	}
	return s, uplink
}

func TestSSHLookupDuringWriterHold(t *testing.T) {
	s, uplink := authReaderFixture(t)
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// Hold the primary writer directly: audit callbacks no longer own it.
	// A short deadline proves the same pool contention.
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM uplinks WHERE fingerprint=?`, uplink.Fingerprint).Scan(&id)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("primary lookup = %v, want deadline", err)
	}
	t.Logf("primary connection wait: %s; waits=%d", time.Since(started), s.db.Stats().WaitCount)
	started = time.Now()
	ctxRead, cancelRead := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancelRead()
	for range 16 {
		got, err := s.UplinkByFingerprint(ctxRead, uplink.Fingerprint)
		if err != nil || got.Identity != uplink.Identity {
			t.Fatalf("lookup during writer hold: %#v, %v", got, err)
		}
	}
	t.Logf("16 fresh auth snapshots during same writer hold: %s", time.Since(started))
}

func TestSSHLookupRevocationAndScopeAreImmediate(t *testing.T) {
	for _, revoke := range []string{"uplink", "credential", "external-process"} {
		t.Run(revoke, func(t *testing.T) {
			s, uplink := authReaderFixture(t)
			if _, err := s.UplinkByFingerprint(t.Context(), uplink.Fingerprint); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetUplinkScope(t.Context(), "operator", uplink.Identity, uplink.Fingerprint, []string{"restricted"}); err != nil {
				t.Fatal(err)
			}
			got, err := s.UplinkByFingerprint(t.Context(), uplink.Fingerprint)
			if err != nil || len(got.Scope) != 1 || got.Scope[0] != "restricted" {
				t.Fatalf("stale scope: %#v, %v", got, err)
			}
			switch revoke {
			case "uplink":
				_, err = s.RevokeUplink(t.Context(), "operator", uplink.Identity, uplink.Fingerprint, "")
			case "credential":
				err = s.RevokeTokenCredential(t.Context(), uplink.TokenCredentialID)
			case "external-process":
				var path string
				err = s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path)
				if err != nil {
					t.Fatal(err)
				}
				other, openErr := OpenAdminClient(t.Context(), path, Options{})
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer func() {
					if err := other.Close(); err != nil {
						t.Error(err)
					}
				}()
				err = other.RevokeTokenCredential(t.Context(), uplink.TokenCredentialID)
			}
			if err != nil {
				t.Fatal(err)
			}
			for range 16 {
				if _, err := s.UplinkByFingerprint(t.Context(), uplink.Fingerprint); !errors.Is(err, ErrNotFound) {
					t.Fatalf("revoked key accepted: %v", err)
				}
			}
		})
	}
}

func TestSSHReadersAreReadOnlyAndBounded(t *testing.T) {
	s, uplink := authReaderFixture(t)
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for range 4 {
		conn, err := s.authDB.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, func() { _ = conn.Close() })
		_, err = conn.ExecContext(t.Context(), `DELETE FROM uplinks`)
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_READONLY {
			t.Fatalf("auth reader write = %v, want SQLITE_READONLY", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.UplinkByFingerprint(ctx, uplink.Fingerprint); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exhausted auth pool = %v", err)
	}
}

func TestSSHReadersRequireServerStartup(t *testing.T) {
	for _, name := range []string{"open", "genesis", "admin", "current", "inspection"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "oberth.sqlite")
			opener := Open
			switch name {
			case "genesis":
				opener = CreateGenesis
			case "admin":
				opener = OpenAdminClient
			case "current", "inspection":
				seed, err := CreateGenesis(t.Context(), path, Options{})
				if err != nil {
					t.Fatal(err)
				}
				if err := seed.Close(); err != nil {
					t.Fatal(err)
				}
				opener = OpenCurrent
				if name == "inspection" {
					opener = InspectCurrent
				}
			}
			s, err := opener(t.Context(), path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			if s.authDB != nil {
				t.Fatal("store opener created SSH readers before server startup")
			}
			if name != "inspection" {
				if err := s.OpenSSHAuthReaders(t.Context()); err != nil {
					t.Fatal(err)
				}
				if s.authDB == nil {
					t.Fatal("server startup did not create SSH readers")
				}
			}
		})
	}
}
