package service

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestIssueWorkspaceMCPAndDashboard(t *testing.T) {
	f := newControlFixture(t)
	issue, err := f.store.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "work", Body: "checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	service := f.api(t)
	call := func(name, raw string) map[string]any {
		t.Helper()
		value, err := service.CallTool(t.Context(), api.Actor{Identity: "writer"}, name, json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err = json.Unmarshal(encoded, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	call("issue_update", fmt.Sprintf(`{"id":%d,"work_status":"active"}`, issue.ID))
	got := call("issue_get", fmt.Sprintf(`{"id":%d,"stale_after":"1ns","active_stale_after":"1ns"}`, issue.ID))
	if got["work_status"] != "active" || got["stale"] != true || got["state"] != "open" {
		t.Fatalf("detail=%v", got)
	}
	page := call("issue_list", `{"work_status":"active","stale_after":"1ns","active_stale_after":"1ns"}`)
	rows := page["issues"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["stale"] != true || page["total"] != float64(1) {
		t.Fatalf("list=%v", page)
	}
	empty := call("issue_list", `{"work_status":"review"}`)
	if len(empty["issues"].([]any)) != 0 || empty["total"] != float64(0) {
		t.Fatalf("status filter=%v", empty)
	}
	view, err := service.Issues(t.Context(), api.Actor{Identity: "writer"}, api.IssueFilter{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(view)
	var dashboard map[string]any
	_ = json.Unmarshal(encoded, &dashboard)
	row := dashboard["Issues"].([]any)[0].(map[string]any)
	if row["work_status"] != "active" || row["stale"] != false {
		t.Fatalf("dashboard=%s", encoded)
	}
	selected := call("issue_get", fmt.Sprintf(`{"id":%d,"fields":["work_status","stale"]}`, issue.ID))
	if selected["work_status"] != "active" || selected["stale"] != false {
		t.Fatalf("compact=%v", selected)
	}
	for _, raw := range []string{`{"work_status":"invalid"}`, `{"stale_after":"0s"}`, `{"active_stale_after":"-1m"}`, `{"stale_after":"bad"}`, `{"stale_after":"1m","active_stale_after":"2m"}`} {
		if _, err := service.CallTool(t.Context(), api.Actor{Identity: "writer"}, "issue_list", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestIssueWorkspaceStaleBoundariesAreReadOnly(t *testing.T) {
	f := newControlFixture(t)
	issue, err := f.store.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "work", Body: "history"})
	if err != nil {
		t.Fatal(err)
	}
	service := f.api(t)
	service.issueStaleAfter = 4 * time.Hour
	service.issueActiveStaleAfter = time.Hour
	for _, tc := range []struct {
		status model.IssueWorkStatus
		state  model.IssueState
		age    time.Duration
		stale  bool
	}{
		{model.IssueTodo, model.IssueOpen, 4 * time.Hour, false},
		{model.IssueTodo, model.IssueOpen, 4*time.Hour + 1, true},
		{model.IssueActive, model.IssueOpen, time.Hour, false},
		{model.IssueActive, model.IssueOpen, time.Hour + 1, true},
		{model.IssueWaiting, model.IssueOpen, 2 * time.Hour, false},
		{model.IssueReview, model.IssueOpen, 5 * time.Hour, true},
		{model.IssueActive, model.IssueClosed, 24 * time.Hour, false},
		{model.IssueActive, model.IssueOpen, -time.Hour, false},
	} {
		value := issue
		value.WorkStatus, value.State = tc.status, tc.state
		service.issueObservedAt = issue.UpdatedAt.Add(tc.age)
		got := service.observeIssue(value)
		if got.Stale != tc.stale {
			t.Errorf("%+v: stale=%v", tc, got.Stale)
		}
	}
	// Exercise actual get/list reads past the configured threshold, including
	// caller-owned locks, and compare the durable record and audit chain.
	if _, err = f.store.AcquireIssueLock(t.Context(), issue.ID, "writer"); err != nil {
		t.Fatal(err)
	}
	beforeAudit, err := f.store.VerifyAuditChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.store.Issue(t.Context(), issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	service.waitClock = clocktesting.NewFakeClock(issue.UpdatedAt.Add(5 * time.Hour))
	actor := api.Actor{Identity: "writer"}
	for _, name := range []string{"issue_get", "issue_get_many", "issue_list"} {
		args := fmt.Sprintf(`{"id":%d}`, issue.ID)
		if name == "issue_get_many" {
			args = fmt.Sprintf(`{"ids":[%d]}`, issue.ID)
		}
		if name == "issue_list" {
			args = `{}`
		}
		value, err := service.CallTool(t.Context(), actor, name, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(value)
		if !strings.Contains(string(encoded), `"stale":true`) {
			t.Fatalf("%s=%s", name, encoded)
		}
	}
	after, err := f.store.Issue(t.Context(), issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterAudit, err := f.store.VerifyAuditChain(t.Context())
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeAudit, afterAudit) {
		t.Fatalf("reads mutated issue/audit: %v", err)
	}
	overridden, err := service.withIssueObservation(issueObservationOptions{StaleAfter: "5m"})
	if err != nil || overridden.issueActiveStaleAfter != 5*time.Minute || service.issueStaleAfter != 4*time.Hour {
		t.Fatalf("override leaked or did not cap active: %v", err)
	}
}
