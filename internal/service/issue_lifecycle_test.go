package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestIssueLifecycleReadSurfacesAndScopedUnlock(t *testing.T) {
	fixture := newScopeFixture(t)
	fixture.service.issueLifecycle = fixture.store
	ctx := context.Background()
	issue, err := fixture.store.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "lock visible", ScopeUpstreamID: fixture.github.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AcquireIssueLock(ctx, issue.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"issue_get", map[string]any{"id": issue.ID}},
		{"issue_get_many", map[string]any{"ids": []int64{issue.ID}}},
		{"issue_list", map[string]any{}},
	} {
		value, err := fixture.call(scopedAdmin, tc.name, tc.args)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(value)
		if !strings.Contains(string(body), `"owner":"owner"`) || !strings.Contains(string(body), `"expires_at"`) {
			t.Fatalf("%s=%s", tc.name, body)
		}
	}
	value, err := fixture.service.Issues(ctx, scopedAdmin, api.IssueFilter{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(value)
	if !strings.Contains(string(body), `"owner":"owner"`) {
		t.Fatalf("HTTP view=%s", body)
	}
	if _, err := fixture.call(scopedActor, "issue_unlock", map[string]any{"id": issue.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin unlock=%v", err)
	}
	hidden, err := fixture.store.CreateManualIssue(ctx, "other", model.ManualIssueSpec{Title: "hidden", ScopeUpstreamID: fixture.codeberg.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.call(scopedAdmin, "issue_unlock", map[string]any{"id": hidden.ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hidden unlock=%v", err)
	}
	if _, err := fixture.call(scopedAdmin, "issue_unlock", map[string]any{"id": issue.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RenewIssueLock(ctx, issue.ID, "owner"); !errors.Is(err, store.ErrLockNotOwned) {
		t.Fatal(err)
	}
}

func TestIssueCloseManyFiftyAndPerIDFailures(t *testing.T) {
	fixture := newScopeFixture(t)
	fixture.service.issueLifecycle = fixture.store
	ctx := context.Background()
	ids := []int64{}
	for i := 0; i < 50; i++ {
		issue, err := fixture.store.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: fmt.Sprintf("bulk %d", i), ScopeUpstreamID: fixture.github.ID})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, issue.ID)
	}
	if _, err := fixture.call(scopedActor, "issue_close_many", map[string]any{"ids": ids, "reason": "obsolete"}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := fixture.call(scopedAdmin, "issue_close_many", map[string]any{"ids": []int64{ids[0], ids[0]}, "reason": "obsolete"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	value, err := fixture.call(scopedAdmin, "issue_close_many", map[string]any{"ids": ids, "reason": "obsolete"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(value)
	if strings.Count(string(body), `"closed":true`) != 50 {
		t.Fatalf("bulk=%s", body)
	}
	for _, id := range ids {
		issue, err := fixture.store.Issue(ctx, id)
		if err != nil || issue.State != model.IssueClosed || !strings.Contains(issue.Body, "obsolete") {
			t.Fatalf("%+v %v", issue, err)
		}
	}
	locked, err := fixture.store.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "locked", ScopeUpstreamID: fixture.github.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AcquireIssueLock(ctx, locked.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	value, err = fixture.call(scopedAdmin, "issue_close_many", map[string]any{"ids": []int64{locked.ID, 999999, ids[0]}, "reason": "obsolete"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(value)
	for _, want := range []string{`"error":"lock_held"`, `"owner":"owner"`, `"error":"not_found"`, `"closed":true`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %s: %s", want, body)
		}
	}
}

func TestIssueLifecycleMutationsRequireAuditGate(t *testing.T) {
	fixture := newScopeFixture(t)
	sentinel := errors.New("audit unavailable")
	fixture.service.mutationGate = func(context.Context) error { return sentinel }
	for _, tool := range []string{"issue_unlock", "issue_close_many"} {
		if _, err := fixture.call(unscopedActor, tool, map[string]any{}); !errors.Is(err, sentinel) {
			t.Fatalf("%s bypassed audit gate: %v", tool, err)
		}
	}
}
