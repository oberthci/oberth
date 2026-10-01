package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestIssueReopenPreservesManualIssueAndVisibility(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	issue, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{Title: "unfinished", Body: "history"})
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(fmt.Sprintf(`{"id":%d}`, issue.ID))
	if _, err := service.CallTool(ctx, actor, "issue_close", args); err != nil {
		t.Fatal(err)
	}
	value, err := service.CallTool(ctx, actor, "issue_reopen", args)
	if err != nil {
		t.Fatalf("reopen manual issue: %v", err)
	}
	got, ok := value.(api.IssueResponse)
	if !ok || got.ID != issue.ID || got.State != "open" || got.Title != issue.Title || got.Body != issue.Body {
		t.Fatalf("reopened issue = %#v", value)
	}
	reopened, err := fixture.store.Issue(ctx, issue.ID)
	if err != nil || reopened.ClosedAt != nil || reopened.Occurrences != issue.Occurrences || !reopened.CreatedAt.Equal(issue.CreatedAt) {
		t.Fatalf("reopen changed history: %#v, %v", reopened, err)
	}
	if _, err := service.CallTool(ctx, actor, "issue_reopen", args); err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
	again, err := fixture.store.Issue(ctx, issue.ID)
	if err != nil || !again.UpdatedAt.Equal(reopened.UpdatedAt) {
		t.Fatalf("retry changed issue: %#v, %v", again, err)
	}
	read, err := service.CallTool(ctx, actor, "issue_get", args)
	if err != nil || read.(api.IssueResponse).State != "open" {
		t.Fatalf("issue_get = %#v, %v", read, err)
	}
	listed, err := service.CallTool(ctx, actor, "issue_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	items := listed.(api.IssueListResponse).Issues
	if len(items) != 1 || items[0].ID != issue.ID || items[0].State != "open" {
		t.Fatalf("issue_list = %#v", listed)
	}
	page, err := fixture.store.ListIssues(ctx, model.IssueListFilter{State: model.IssueOpen})
	if err != nil || len(page.Issues) != 1 || page.Issues[0].ID != issue.ID {
		t.Fatalf("open issue list = %#v, %v", page, err)
	}
	if _, err := fixture.store.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIssueReopenDeniesCIInvalidInputAndOtherOwners(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	ci, err := fixture.store.UpsertCIIssue(ctx, actor.Identity, fixture.repo.ID, "feature", "red", "failure")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CloseCIIssue(ctx, actor.Identity, ci.ID, "resolved"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args string
		want error
	}{
		{fmt.Sprintf(`{"id":%d}`, ci.ID), ErrInvalidInput},
		{`{"id":999999}`, store.ErrNotFound},
		{`{"id":0}`, ErrInvalidInput},
		{`{"id":-1}`, ErrInvalidInput},
		{`{}`, ErrInvalidInput},
		{`{"id":1,"state":"open"}`, ErrInvalidInput},
	} {
		if _, err := service.CallTool(ctx, actor, "issue_reopen", json.RawMessage(tc.args)); !errors.Is(err, tc.want) {
			t.Errorf("reopen %s: %v, want %v", tc.args, err, tc.want)
		}
	}
	stillClosed, err := fixture.store.Issue(ctx, ci.ID)
	if err != nil || stillClosed.State != model.IssueClosed {
		t.Fatalf("CI projection changed: %#v, %v", stillClosed, err)
	}
	manual, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{Title: "locked"})
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(fmt.Sprintf(`{"id":%d}`, manual.ID))
	if _, err := service.CallTool(ctx, actor, "issue_close", args); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AcquireIssueLock(ctx, manual.ID, "other@host"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CallTool(ctx, actor, "issue_reopen", args); !errors.Is(err, store.ErrLockHeld) {
		t.Fatalf("competing owner error = %v", err)
	}
	if _, err := service.CallTool(ctx, api.Actor{}, "issue_reopen", args); err == nil {
		t.Fatal("anonymous reopen accepted")
	}
}

func TestIssueReopenRequiresMutationGate(t *testing.T) {
	fixture := newControlFixture(t)
	service := fixture.api(t)
	gateErr := errors.New("audit unavailable")
	service.mutationGate = func(context.Context) error { return gateErr }
	_, err := service.CallTool(context.Background(), api.Actor{Identity: "agent@host"}, "issue_reopen", json.RawMessage(`{"id":1}`))
	if !errors.Is(err, gateErr) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reopen bypassed mutation gate: %v", err)
	}
}
