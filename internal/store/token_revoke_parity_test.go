package store

import (
	"github.com/oberthci/oberth/internal/model"
	"testing"
	"time"
)

func TestUplinkRemovalClearsPendingCredential(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	credential, err := s.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "pending", Digest: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := s.RegisterUplink(t.Context(), "operator", model.UplinkSpec{Identity: "worker", Fingerprint: "SHA256:pending", TokenCredentialID: credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RemoveUplink(t.Context(), "operator", uplink.Identity, uplink.Fingerprint); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err = s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_token_credentials WHERE token_credential_id=?`, credential.ID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatal("production removal leaves a pending activation, unlike fixture revocation")
	}
}
