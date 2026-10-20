package store

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestListAuditActionsFiltersPagesAndLinks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	ctx := context.Background()
	for i, spec := range []struct{ actor, action string }{
		{"a@host", "alpha.one"}, {"b@host", "alpha.two"}, {"a@host", "beta.one"}, {"a@host", "alpha.three"},
	} {
		now = now.Add(time.Hour)
		if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: spec.actor, Action: spec.action, ResourceType: "t", ResourceID: string(rune('0' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Hour)
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "a@host", Action: "secret_access.grant", ResourceType: "secret_access", ResourceID: "9", Details: `{"repo":"github/org/repo"}`}); err != nil {
		t.Fatal(err)
	}
	byRepo, _, err := database.ListAuditActions(ctx, model.AuditListFilter{RepoName: "github/org/repo"})
	if err != nil || len(byRepo) != 1 || byRepo[0].Action != "secret_access.grant" {
		t.Fatalf("repo filter = %+v err %v", byRepo, err)
	}
	all, head, err := database.ListAuditActions(ctx, model.AuditListFilter{})
	if err != nil || head != 5 || len(all) != 5 || all[0].ID != 5 || all[4].ID != 1 {
		t.Fatalf("all = %+v head %d err %v", all, head, err)
	}
	for i := 0; i+1 < len(all); i++ {
		if string(all[i].PreviousSHA256) != string(all[i+1].SHA256) {
			t.Fatalf("entry %d does not link to %d", all[i].ID, all[i+1].ID)
		}
	}
	got, _, err := database.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "alpha.", Actor: "a@host"})
	if err != nil || len(got) != 2 || got[0].Action != "alpha.three" || got[1].Action != "alpha.one" {
		t.Fatalf("prefix+actor = %+v err %v", got, err)
	}
	got, _, err = database.ListAuditActions(ctx, model.AuditListFilter{Since: now.Add(-150 * time.Minute), Until: now.Add(-90 * time.Minute)})
	if err != nil || len(got) != 1 || got[0].Action != "beta.one" {
		t.Fatalf("time range = %+v err %v", got, err)
	}
	got, _, err = database.ListAuditActions(ctx, model.AuditListFilter{Before: 3, Limit: 1})
	if err != nil || len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("cursor = %+v err %v", got, err)
	}
}

func TestRecordOperationAuditAcceptsOnlyLifecycleActions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	ctx := context.Background()
	if _, err := database.RecordOperationAudit(ctx, "admin@localhost", "secret_access.grant", "{}"); err == nil {
		t.Fatal("arbitrary action was recorded")
	}
	action, err := database.RecordOperationAudit(ctx, "admin@localhost", "backup.completed", `{"archive_sha256":"ab"}`)
	if err != nil || action.ID != 1 || action.ResourceType != "system" {
		t.Fatalf("action = %+v err %v", action, err)
	}
	if _, err := database.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}
