package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// T17: TestTailAuditActionReturnsVerifiedTail
func TestTailAuditActionReturnsVerifiedTail(t *testing.T) {
	t.Run("multi-action chain returns last action", func(t *testing.T) {
		now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
		s := testStore(t, &now)
		ctx := context.Background()
		for _, id := range []string{"first", "second", "third"} {
			now = now.Add(time.Second)
			if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
				Actor: "agent@host", Action: "test", ResourceType: "tail", ResourceID: id,
			}); err != nil {
				t.Fatal(err)
			}
		}
		tail, err := s.TailAuditAction(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if tail.ID != 3 || tail.ResourceID != "third" {
			t.Fatalf("tail = %+v, want ID=3 ResourceID=third", tail)
		}
	})

	t.Run("empty chain returns error", func(t *testing.T) {
		now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
		s := testStore(t, &now)
		ctx := context.Background()
		_, err := s.TailAuditAction(ctx)
		if err == nil || !strings.Contains(err.Error(), "audit chain is empty") {
			t.Fatalf("empty chain error = %v, want 'audit chain is empty'", err)
		}
	})

	t.Run("tampered tail fails verification", func(t *testing.T) {
		now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
		s := testStore(t, &now)
		ctx := context.Background()
		for _, id := range []string{"one", "two"} {
			now = now.Add(time.Second)
			if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
				Actor: "agent@host", Action: "test", ResourceType: "tail", ResourceID: id,
			}); err != nil {
				t.Fatal(err)
			}
		}
		// Tamper: drop the no-update trigger and change the tail.
		if _, err := s.db.ExecContext(ctx, `DROP TRIGGER audit_actions_no_update`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE audit_actions SET details = '{"tampered":true}' WHERE id = 2`); err != nil {
			t.Fatal(err)
		}
		_, err := s.TailAuditAction(ctx)
		if err == nil || !strings.Contains(err.Error(), "hash is invalid") {
			t.Fatalf("tampered tail error = %v, want hash verification failure", err)
		}
	})
}
