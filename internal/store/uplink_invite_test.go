package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/invite"
)

func TestUplinkInviteAtomicEnrollment(t *testing.T) {
	now := time.Now()
	database := testStore(t, &now)
	_, digest, err := invite.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	pin := strings.Repeat("a", 64)
	address := "https://oberth.example.test/mcp"
	binding, err := database.CreateUplinkInvite(t.Context(), "owner@example.test", digest, "agent@example.test", address, pin, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var issued atomic.Int32
	enroll := func(e *InviteEnrollment) (string, error) {
		issued.Add(1)
		u, err := e.Uplink()
		if err != nil {
			return "", err
		}
		if err := e.BindClientPin(t.Context(), u.Identity, u.ID, strings.Repeat("b", 64), now.Add(time.Hour)); err != nil {
			return "", err
		}
		return "public certificate", nil
	}
	for _, scenario := range []string{"wrong-pin", "wrong-url", "issuance-failure"} {
		p, u := pin, address
		callback := enroll
		switch scenario {
		case "wrong-pin":
			p = strings.Repeat("c", 64)
		case "wrong-url":
			u = "https://other.example.test/mcp"
		case "issuance-failure":
			callback = func(*InviteEnrollment) (string, error) { return "", errors.New("signer unavailable") }
		}
		if _, _, err := database.RedeemUplinkInvite(t.Context(), digest, u, p, "SHA256:local", callback); err == nil {
			t.Fatal("unsafe redemption accepted")
		}
	}
	var registered int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM uplinks WHERE identity=?`, binding.Identity).Scan(&registered); err != nil || registered != 0 {
		t.Fatal("failed redemption left registration", err)
	}
	issued.Store(0)
	var success atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, _, err := database.RedeemUplinkInvite(context.Background(), digest, address, pin, "SHA256:local", enroll); err == nil {
				success.Add(1)
			}
		}()
	}
	group.Wait()
	if success.Load() != 1 || issued.Load() != 1 {
		t.Fatal("single-use invite had multiple winners")
	}
	u, err := database.UplinkByClientPin(t.Context(), strings.Repeat("b", 64))
	if err != nil || u.Identity != binding.Identity {
		t.Fatal("certificate pin not active", err)
	}
	var bearerCount int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM token_credentials WHERE certificate_only=0`).Scan(&bearerCount); err != nil || bearerCount != 0 {
		t.Fatal("redemption minted a bearer credential", err)
	}
	var audit string
	if err := database.db.QueryRow(`SELECT details FROM audit_actions WHERE action='uplink.invite.redeem'`).Scan(&audit); err != nil || strings.Contains(audit, digest) || !strings.Contains(audit, `"auth_method":"invite"`) {
		t.Fatal("invalid enrollment audit receipt", err)
	}
}
func TestUplinkInviteExpiryAndNoIssuanceRollback(t *testing.T) {
	now := time.Now()
	database := testStore(t, &now)
	_, digest, err := invite.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	pin := strings.Repeat("a", 64)
	address := "https://oberth.example.test/mcp"
	if _, err := database.CreateUplinkInvite(t.Context(), "owner@example.test", digest, "agent@example.test", address, pin, nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	noPin := func(*InviteEnrollment) (string, error) { return "not a bound certificate", nil }
	if _, _, err := database.RedeemUplinkInvite(t.Context(), digest, address, pin, "SHA256:local", noPin); !errors.Is(err, ErrInvalidState) {
		t.Fatal("enrollment without a pin committed", err)
	}
	var consumed bool
	if err := database.db.QueryRow(`SELECT redeemed_at IS NOT NULL FROM uplink_invites`).Scan(&consumed); err != nil || consumed {
		t.Fatal("failed issuance consumed invite", err)
	}
	now = now.Add(time.Minute)
	if _, _, err := database.RedeemUplinkInvite(t.Context(), digest, address, pin, "SHA256:local", noPin); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired invite accepted", err)
	}
}

func TestUplinkInviteAuditFailureRollsBackEveryBinding(t *testing.T) {
	now := time.Now()
	database := testStore(t, &now)
	_, digest, err := invite.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	pin := strings.Repeat("a", 64)
	address := "https://oberth.example.test/mcp"
	if _, err := database.CreateUplinkInvite(t.Context(), "owner@example.test", digest, "agent@example.test", address, pin, nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`CREATE TRIGGER refuse_invite_audit BEFORE INSERT ON audit_actions WHEN NEW.action='uplink.invite.redeem' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	enroll := func(e *InviteEnrollment) (string, error) {
		u, err := e.Uplink()
		if err != nil {
			return "", err
		}
		if err := e.BindClientPin(t.Context(), u.Identity, u.ID, strings.Repeat("b", 64), now.Add(time.Hour)); err != nil {
			return "", err
		}
		return "public certificate", nil
	}
	if _, _, err := database.RedeemUplinkInvite(t.Context(), digest, address, pin, "SHA256:local", enroll); err == nil {
		t.Fatal("redemption ignored audit failure")
	}
	for _, table := range []string{"uplinks", "uplink_client_pins", "token_credentials"} {
		var count int
		if err := database.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("failed audit left enrollment state", err)
		}
	}
	if _, err := database.db.Exec(`DROP TRIGGER refuse_invite_audit`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.RedeemUplinkInvite(t.Context(), digest, address, pin, "SHA256:local", enroll); err != nil {
		t.Fatal("failed audit consumed invite", err)
	}
}
