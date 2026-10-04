package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type bulkIssueReader struct {
	IssueRepository // Unexpected writes/lock renewals panic rather than passing unnoticed.
	issues          map[int64]model.Issue
	calls           []int64
	fail            error
}

func (reader *bulkIssueReader) Issue(_ context.Context, id int64) (model.Issue, error) {
	reader.calls = append(reader.calls, id)
	if reader.fail != nil {
		return model.Issue{}, reader.fail
	}
	issue, ok := reader.issues[id]
	if !ok {
		return model.Issue{}, store.ErrNotFound
	}
	return issue, nil
}
func callIssueBatch(t *testing.T, reader *bulkIssueReader, ids []int64) api.IssueGetManyResponse {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"ids": ids})
	if err != nil {
		t.Fatal(err)
	}
	value, err := (&API{issues: reader}).CallTool(context.Background(), api.Actor{Identity: "reader"}, "issue_get_many", raw)
	if err != nil {
		t.Fatal(err)
	}
	return value.(api.IssueGetManyResponse)
}
func TestIssueGetManyOrderMissingAndReadOnly(t *testing.T) {
	reader := &bulkIssueReader{issues: map[int64]model.Issue{1: {ID: 1, Title: "one", Body: "whole\nbody", Kind: model.IssueManual, State: model.IssueClosed}, 2: {ID: 2, Title: "two", Kind: model.IssueCI, State: model.IssueOpen, RepoID: 3}}}
	got := callIssueBatch(t, reader, []int64{2, 999, 1})
	if !reflect.DeepEqual(reader.calls, []int64{2, 999, 1}) || len(got.Results) != 3 {
		t.Fatalf("order/reads: %#v %#v", got, reader.calls)
	}
	if got.Results[0].Issue == nil || *got.Results[0].Issue != wireIssue(reader.issues[2]) || got.Results[1].Error != "not_found" || got.Results[1].Issue != nil || got.Results[2].Issue == nil || *got.Results[2].Issue != wireIssue(reader.issues[1]) {
		t.Fatalf("records: %#v", got)
	}
}
func TestIssueGetManyRejectsInvalidBeforeReading(t *testing.T) {
	fiftyOne := make([]int64, 51)
	for i := range fiftyOne {
		fiftyOne[i] = int64(i + 1)
	}
	tooMany, _ := json.Marshal(map[string]any{"ids": fiftyOne})
	for _, raw := range []string{`{}`, `{"ids":null}`, `{"ids":[]}`, `{"ids":[1,0]}`, `{"ids":[1,-2]}`, `{"ids":[1,1]}`, `{"ids":[1,1.5]}`, `{"ids":["1"]}`, `{"ids":[9223372036854775808]}`, `{"ids":[1],"unknown":true}`, string(tooMany)} {
		reader := &bulkIssueReader{}
		_, err := (&API{issues: reader}).CallTool(context.Background(), api.Actor{Identity: "reader"}, "issue_get_many", json.RawMessage(raw))
		if !errors.Is(err, ErrInvalidInput) || len(reader.calls) != 0 {
			t.Errorf("input %s: %v, reads %v", raw, err, reader.calls)
		}
	}
	reader := &bulkIssueReader{}
	ids := make([]int64, 50)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	if got := callIssueBatch(t, reader, ids); len(got.Results) != 50 {
		t.Fatal("50 IDs rejected")
	}
}
func TestIssueGetManyPropagatesFailureAndCancellation(t *testing.T) {
	failure := errors.New("private storage failure")
	reader := &bulkIssueReader{fail: failure}
	service := &API{issues: reader}
	_, err := service.CallTool(context.Background(), api.Actor{Identity: "reader"}, "issue_get_many", json.RawMessage(`{"ids":[1,2]}`))
	if !errors.Is(err, failure) || len(reader.calls) != 1 {
		t.Fatalf("failure: %v %v", err, reader.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader.calls = nil
	_, err = service.CallTool(ctx, api.Actor{Identity: "reader"}, "issue_get_many", json.RawMessage(`{"ids":[1]}`))
	if !errors.Is(err, context.Canceled) || len(reader.calls) != 0 {
		t.Fatalf("cancel: %v %v", err, reader.calls)
	}
}
func TestIssueGetManyEncodedBudgetBoundary(t *testing.T) {
	base := model.Issue{ID: 1, Kind: model.IssueManual, State: model.IssueOpen, Title: "exact"}
	wire := wireIssue(base)
	encoded, _ := json.Marshal(api.IssueGetManyResponse{Results: []api.IssueGetManyResult{{ID: 1, Issue: &wire}}})
	bodyLength := api.IssueGetManyMaximumBytes - len(encoded)
	for _, extra := range []int{0, 1} {
		issue := base
		issue.Body = strings.Repeat("x", bodyLength+extra)
		got := callIssueBatch(t, &bulkIssueReader{issues: map[int64]model.Issue{1: issue}}, []int64{1})
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if extra == 0 && (len(encoded) != api.IssueGetManyMaximumBytes || got.Results[0].Issue == nil || got.Results[0].Issue.Body != issue.Body) {
			t.Fatalf("exact fit: %d %#v", len(encoded), got.Results[0].Error)
		}
		if extra == 1 && (got.Results[0].Error != "response_limit" || got.Results[0].Issue != nil) {
			t.Fatal("one-byte overflow not explicit")
		}
	}
}
func TestIssueGetManyReservesEveryIDAndAllowsLaterSmallRecords(t *testing.T) {
	reader := &bulkIssueReader{issues: map[int64]model.Issue{
		1: {ID: 1, Body: strings.Repeat("x", api.IssueGetManyMaximumBytes+1)},
		2: {ID: 2, Body: strings.Repeat("\x00\"\\<", 30000)}, // JSON escaping, not raw size, exhausts budget.
		3: {ID: 3, Body: strings.Repeat("x", 200000)},
		4: {ID: 4, Body: strings.Repeat("x", 100000)},
		5: {ID: 5, Body: "later complete record"},
	}}
	ids := make([]int64, 50)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	got := callIssueBatch(t, reader, ids)
	encoded, _ := json.Marshal(got)
	if len(encoded) > api.IssueGetManyMaximumBytes || len(got.Results) != 50 {
		t.Fatalf("budget/rows: %d/%d", len(encoded), len(got.Results))
	}
	for _, i := range []int{0, 1, 3} {
		if got.Results[i].Error != "response_limit" || got.Results[i].Issue != nil {
			t.Fatalf("overflow %d hidden", i)
		}
	}
	if got.Results[2].Issue == nil || got.Results[4].Issue == nil || got.Results[4].Issue.Body != reader.issues[5].Body {
		t.Fatal("later records lost")
	}
	for i := 5; i < 50; i++ {
		if got.Results[i].ID != ids[i] || got.Results[i].Error != "not_found" {
			t.Fatalf("missing row %d", i)
		}
	}
}

func TestIssueListFiltersBeforePaginationAndPreservesDefaults(t *testing.T) {
	ctx := context.Background()
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "reader"}
	var openIDs []int64
	for i := 0; i < 81; i++ {
		issue, err := fixture.store.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{Title: fmt.Sprintf("component-%d", i), Body: "body"})
		if err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if _, err := service.CallTool(ctx, actor, "issue_close", json.RawMessage(fmt.Sprintf(`{"id":%d}`, issue.ID))); err != nil {
				t.Fatal(err)
			}
		} else {
			openIDs = append([]int64{issue.ID}, openIDs...)
		}
	}
	ci, err := fixture.store.UpsertCIIssue(ctx, actor.Identity, fixture.repo.ID, "feature", "red", "failure")
	if err != nil {
		t.Fatal(err)
	}
	list := func(raw string) api.IssueListResponse {
		t.Helper()
		value, err := service.CallTool(ctx, actor, "issue_list", json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		return value.(api.IssueListResponse)
	}
	original := list(`{}`)
	all := list(`{"state":"all","limit":50}`)
	if !reflect.DeepEqual(original, all) || len(original.Issues) != 50 || original.NextBefore == 0 || original.Issues[0].ID != ci.ID {
		t.Fatalf("defaults: %#v", original)
	}
	for _, limit := range []int{5, 10, 50} {
		page := list(fmt.Sprintf(`{"limit":%d}`, limit))
		if len(page.Issues) != limit || page.NextBefore != page.Issues[limit-1].ID {
			t.Fatalf("limit %d: %#v", limit, page)
		}
	}
	var gotIDs []int64
	var before int64
	for {
		page := list(fmt.Sprintf(`{"state":"open","kind":"manual","limit":5,"before":%d}`, before))
		for _, issue := range page.Issues {
			if issue.State != "open" || issue.Kind != "manual" {
				t.Fatal("filter leakage")
			}
			gotIDs = append(gotIDs, issue.ID)
		}
		if page.NextBefore == 0 {
			break
		}
		if len(page.Issues) != 5 || page.NextBefore >= before && before != 0 {
			t.Fatal("invalid cursor")
		}
		before = page.NextBefore
	}
	if !reflect.DeepEqual(gotIDs, openIDs) {
		t.Fatalf("filtered enumeration %v, want %v", gotIDs, openIDs)
	}
	repo := list(fmt.Sprintf(`{"repo":%q,"state":"open"}`, fixture.repo.Name))
	if len(repo.Issues) != 1 || repo.Issues[0].ID != ci.ID {
		t.Fatalf("global manual entered repo filter: %#v", repo)
	}
	closed := list(`{"state":"closed"}`)
	if len(closed.Issues) != 27 || closed.NextBefore != 0 {
		t.Fatalf("closed: %#v", closed)
	}
	for _, raw := range []string{`{"limit":0}`, `{"limit":51}`, `{"limit":-1}`, `{"limit":1.5}`, `{"state":"pending"}`, `{"kind":"bug"}`, `{"before":-1}`, `{"unknown":1}`} {
		if _, err := service.CallTool(ctx, actor, "issue_list", json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	if _, err := service.CallTool(ctx, actor, "issue_list", json.RawMessage(`{"repo":"missing"}`)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing repo: %v", err)
	}
}
