package store

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAdministrativeRegistrationsAreAtomicWithAudit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	upstream, err := database.RegisterUpstream(context.Background(), "admin@localhost", model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/cloudtaser",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := database.CreateTokenCredential(context.Background(), model.TokenCredentialSpec{Name: "agent@host", Digest: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := database.RegisterUplink(context.Background(), "admin@localhost", model.UplinkSpec{
		Fingerprint: "SHA256:key", Identity: "agent@host", TokenCredentialID: pending.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if upstream.ID <= 0 || uplink.TokenCredentialID != pending.ID || uplink.AuthActor != "admin@localhost" {
		t.Fatalf("upstream/uplink = %+v / %+v", upstream, uplink)
	}
	var actions int
	if err := database.db.QueryRowContext(context.Background(), `
SELECT count(*) FROM audit_actions WHERE action IN ('upstream.register', 'uplink.register')`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != 2 {
		t.Fatalf("administrative audit actions = %d", actions)
	}
}
