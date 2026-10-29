package auditanchor

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestLocalCycleCannotResetVerifiedPrefixAfterRollback(t *testing.T) {
	for _, timestamp := range []bool{false, true} {
		name := "local"
		if timestamp {
			name = "timestamp"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			path := filepath.Join(t.TempDir(), "audit.db")
			database, err := store.OpenAdminClient(t.Context(), path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = database.Close() }()
			appendAction := func() {
				t.Helper()
				if _, err := database.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "test", Action: "append", ResourceType: "test"}); err != nil {
					t.Fatal(err)
				}
			}
			appendAction()
			config := ManagerConfig{Store: database, Interval: time.Minute, MaxAge: time.Hour}
			if timestamp {
				config.Authority = &fakeAuthority{now: &now}
			}
			manager, err := NewManager(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Initialize(t.Context()); err != nil {
				t.Fatal(err)
			}
			appendAction()
			if err := manager.AllowMutation(t.Context()); err != nil {
				t.Fatal(err)
			}
			if id, _ := manager.getVerifiedHead(); id != 2 {
				t.Fatalf("gate did not advance the process pin: %d", id)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			if _, err := raw.ExecContext(t.Context(), `DROP TRIGGER audit_actions_no_delete; DELETE FROM audit_actions WHERE id=2`); err != nil {
				t.Fatal(err)
			}
			if _, err := database.VerifyAuditChain(t.Context()); err != nil {
				t.Fatalf("test needs a self-consistent shorter chain: %v", err)
			}
			if err := manager.AllowMutation(t.Context()); err == nil {
				t.Fatal("gate accepted rollback")
			}
			manager.cycle(t.Context())
			if err := manager.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "verify prior local prefix") {
				t.Fatalf("cycle cleared rollback refusal: %v", err)
			}
			if id, _ := manager.getVerifiedHead(); id != 2 {
				t.Fatalf("cycle reset prefix pin to %d", id)
			}
		})
	}
}
