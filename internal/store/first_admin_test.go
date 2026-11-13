package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestFirstAdminSecurity(t *testing.T) {
	t.Run("LastAdminCannotBeScopedDemotedOrReplaced", testLastAdminMutationGuards)
	t.Run("SameKeyRedeliveryAndRecovery", testFirstAdminRedelivery)
	t.Run("EveryBindingRollsBack", testFirstAdminRollback)
}

func testLastAdminMutationGuards(t *testing.T) {
	for _, action := range []string{"scope", "demote", "replace"} {
		t.Run(action, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			u, _ := lifecycleUplink(t, s, "owner", "SHA256:owner", true)
			digest := sha256.Sum256([]byte(action))
			token, err := s.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: u.Identity, Digest: digest[:]})
			if err != nil {
				t.Fatal(err)
			}
			mutate := func() error {
				if action == "scope" {
					_, err := s.SetUplinkScope(t.Context(), "owner", u.Identity, u.Fingerprint, []string{"forge"})
					return err
				}
				_, err := s.RegisterUplink(t.Context(), "owner", model.UplinkSpec{Identity: u.Identity, Fingerprint: u.Fingerprint, TokenCredentialID: token.ID, Admin: action != "demote"})
				return err
			}
			if err := mutate(); !errors.Is(err, ErrInvalidState) {
				t.Fatal("last active admin mutation accepted", action, err)
			}
			lifecycleUplink(t, s, "second", "SHA256:second", true)
			if err := mutate(); err != nil {
				t.Fatal("another active admin should permit mutation", action, err)
			}
		})
	}
}

func firstAdminTestIssue(t *testing.T, now *time.Time, pin string, fail bool) func(*FirstAdminEnrollment) error {
	t.Helper()
	return func(e *FirstAdminEnrollment) error {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", pin, now.UnixNano())))
		token, err := e.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: e.spec.Identity, Digest: digest[:]})
		if err != nil {
			return err
		}
		if err := e.Register(t.Context(), token); err != nil {
			return err
		}
		if _, err := e.ActivateTokenCredential(t.Context(), token.ID); err != nil {
			return err
		}
		if err := e.BindClientPin(t.Context(), "exec", e.uplink.ID, token.ID, pin, now.Add(time.Hour)); err != nil {
			return err
		}
		if fail {
			return errors.New("interrupted issuance")
		}
		return nil
	}
}

func testFirstAdminRedelivery(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	pin := strings.Repeat("a", 64)
	create := func(identity, fingerprint, pin string, bearer bool) error {
		return s.CreateFirstAdmin(t.Context(), "exec", identity, fingerprint, pin, bearer, model.DefaultUplinkTokenPolicy(), firstAdminTestIssue(t, &now, pin, false))
	}
	if err := create("owner", "SHA256:owner", pin, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := create("owner", "SHA256:owner", pin, true); err != nil {
		t.Fatal("same key retry refused", err)
	}
	for _, test := range []struct{ identity, fingerprint, pin string }{{"owner", "SHA256:owner", strings.Repeat("b", 64)}, {"other", "SHA256:owner", pin}, {"owner", "SHA256:other", pin}} {
		if err := create(test.identity, test.fingerprint, test.pin, true); !errors.Is(err, ErrInvalidState) {
			t.Fatal("different enrollment accepted while active admin exists", err)
		}
	}
	now = now.Add(365 * 24 * time.Hour)
	if err := create("owner", "SHA256:owner", strings.Repeat("b", 64), false); err != nil {
		t.Fatal("expired sole admin cannot recover", err)
	}
	now = now.Add(2 * time.Hour)
	if err := create("owner", "SHA256:owner", strings.Repeat("c", 64), false); err != nil {
		t.Fatal("lost sole certificate admin cannot recover", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM uplinks`).Scan(&count); err != nil || count != 1 {
		t.Fatal("retry created additional admin", count, err)
	}
}

func testFirstAdminRollback(t *testing.T) {
	for _, auditFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(auditFailure), func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			pin := strings.Repeat("d", 64)
			if auditFailure {
				if _, err := s.db.Exec(`CREATE TRIGGER refuse_first_admin_audit BEFORE INSERT ON audit_actions WHEN NEW.action='uplink.first-admin' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err := s.CreateFirstAdmin(t.Context(), "exec", "owner", "SHA256:owner", pin, true, model.DefaultUplinkTokenPolicy(), firstAdminTestIssue(t, &now, pin, !auditFailure))
			if err == nil {
				t.Fatal("failed enrollment committed")
			}
			for _, table := range []string{"uplinks", "token_credentials", "pending_token_credentials", "uplink_client_pins"} {
				var n int
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
					t.Fatal("failed enrollment left state", table, n, err)
				}
			}
		})
	}
}
