package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func authReaderFixture(t *testing.T) (*Store, model.Uplink) {
	t.Helper()
	now := time.Now()
	s := testStore(t, &now)
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

func TestSSHLookupDuringAuditGateHold(t *testing.T) {
	s, uplink := authReaderFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, _, err := s.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, func(model.AuditHead, model.AuditAnchor) error {
			close(entered)
			<-release
			return nil
		})
		finished <- err
	}()
	<-entered
	defer func() {
		close(release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	// Reproduce the old path with the real audit transaction holding the sole
	// primary connection. Use the production five-second identity deadline.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM uplinks WHERE fingerprint=?`, uplink.Fingerprint).Scan(&id)
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
	for range 4 {
		conn, err := s.authDB.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, func() { _ = conn.Close() })
		if _, err := conn.ExecContext(t.Context(), `DELETE FROM uplinks`); err == nil {
			t.Fatal("auth reader allowed a write")
		}
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.UplinkByFingerprint(ctx, uplink.Fingerprint); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exhausted auth pool = %v", err)
	}
}
