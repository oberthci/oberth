package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func holdGeneralReaders(t *testing.T, s *Store) {
	t.Helper()
	for range liveReadConnections {
		conn, err := s.readDB.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.ListUpstreams(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("general pool not saturated: %v", err)
	}
}

func TestTokenAuthReserveSurvivesGeneralPoolAndWriterSaturation(t *testing.T) {
	s, uplink := authReaderFixture(t)
	// A concurrent-touch no-op must not fall back onto dashboard readers either.
	if err := s.TouchTokenCredential(t.Context(), uplink.TokenCredentialID); err != nil {
		t.Fatal(err)
	}
	holdGeneralReaders(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := s.TouchTokenCredential(ctx, uplink.TokenCredentialID); err != nil {
		t.Fatalf("no-op touch queued behind dashboard readers: %v", err)
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for range 8 {
		got, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32))
		if err != nil || got.ID != uplink.ID || got.TokenCredential.LastUsedAt == nil {
			t.Fatalf("token identity queued behind dashboard/writer: %+v %v", got, err)
		}
		credential, err := s.TokenCredentialByDigest(ctx, make([]byte, 32))
		if err != nil || credential.ID != uplink.TokenCredentialID {
			t.Fatalf("credential lookup queued: %+v %v", credential, err)
		}
		if _, err := s.UplinkByFingerprint(ctx, uplink.Fingerprint); err != nil {
			t.Fatalf("SSH reserve regressed: %v", err)
		}
	}
}

func TestTokenAuthReserveReadsFreshScopeAndRevocation(t *testing.T) {
	for _, kind := range []string{"uplink", "credential", "external-process"} {
		t.Run(kind, func(t *testing.T) {
			s, uplink := authReaderFixture(t)
			holdGeneralReaders(t, s)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetUplinkScope(ctx, "operator", uplink.Identity, uplink.Fingerprint, []string{"restricted"}); err != nil {
				t.Fatal(err)
			}
			got, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32))
			if err != nil || len(got.Scope) != 1 || got.Scope[0] != "restricted" {
				t.Fatalf("cached or incomplete scope: %+v %v", got, err)
			}
			switch kind {
			case "uplink":
				_, err = s.RevokeUplink(ctx, "operator", uplink.Identity, uplink.Fingerprint, "")
			case "credential":
				err = s.RevokeTokenCredential(ctx, uplink.TokenCredentialID)
			case "external-process":
				other, openErr := OpenAdminClient(ctx, s.livePath, Options{})
				if openErr != nil {
					t.Fatal(openErr)
				}
				err = other.RevokeTokenCredential(ctx, uplink.TokenCredentialID)
				if closeErr := other.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			for range 4 {
				if _, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32)); !errors.Is(err, ErrNotFound) {
					t.Fatalf("revoked token accepted: %v", err)
				}
			}
		})
	}
}

func TestTokenAuthReserveIsBoundedAndClosed(t *testing.T) {
	s, _ := authReaderFixture(t)
	var held []*sql.Conn
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	for range 4 {
		conn, err := s.authDB.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
		var only int
		if err := conn.QueryRowContext(t.Context(), "PRAGMA query_only").Scan(&only); err != nil || only != 1 {
			t.Fatalf("auth pool is writable: query_only=%d %v", only, err)
		}
		if _, err := conn.ExecContext(t.Context(), "DELETE FROM token_credentials"); err == nil {
			t.Fatal("auth reader accepted write")
		}
	}
	for _, lookup := range []func(context.Context) error{
		func(ctx context.Context) error {
			_, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32))
			return err
		},
		func(ctx context.Context) error {
			_, err := s.TokenCredentialByDigest(ctx, make([]byte, 32))
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		err := lookup(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("exhausted auth reserve fell back to general pool: %v", err)
		}
	}
	for _, conn := range held {
		_ = conn.Close()
	}
	held = nil
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticatedUplinkByDigest(t.Context(), make([]byte, 32)); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed auth pool remained usable: %v", err)
	}
}
