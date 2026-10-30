package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func readAcknowledgedIssue(t *testing.T, service *API, actor api.Actor, value any) api.IssueResponse {
	t.Helper()
	ack, ok := value.(api.IssueMutationResponse)
	if !ok {
		t.Fatalf("mutation returned %T, want compact acknowledgment", value)
	}
	raw := json.RawMessage(fmt.Sprintf(`{"id":%d}`, ack.ID))
	read, err := service.CallTool(t.Context(), actor, "issue_get", raw)
	if err != nil {
		t.Fatal(err)
	}
	issue := read.(api.IssueResponse)
	if ack.State != issue.State || ack.UpdatedAt != issue.UpdatedAt || ack.Occurrences != int64(issue.Occurrences) {
		t.Fatalf("acknowledgment does not match persisted issue: %#v / %#v", ack, issue)
	}
	return issue
}

func TestIssueMutationsReturnOnlyAcknowledgments(t *testing.T) {
	fixture := newControlFixture(t)
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	issue, err := fixture.store.CreateManualIssue(t.Context(), actor.Identity, model.ManualIssueSpec{
		Title: "large issue", Body: strings.Repeat("original body\n", 10000),
	})
	if err != nil {
		t.Fatal(err)
	}
	text := "checkpoint 界"
	for _, test := range []struct {
		name     string
		args     map[string]any
		appended int
	}{
		{"issue_append", map[string]any{"id": issue.ID, "text": text}, len(text)},
		{"issue_update", map[string]any{"id": issue.ID, "title": "updated", "body": issue.Body}, 0},
		{"issue_close", map[string]any{"id": issue.ID}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(test.args)
			value, err := service.CallTool(t.Context(), actor, test.name, raw)
			if err != nil {
				t.Fatal(err)
			}
			readAcknowledgedIssue(t, service, actor, value)
			ack := value.(api.IssueMutationResponse)
			if ack.AppendedBytes != test.appended {
				t.Fatalf("appended_bytes = %d, want %d", ack.AppendedBytes, test.appended)
			}
			encoded, _ := json.Marshal(value)
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 5 || len(encoded) > 256 || fields["id"] == nil || fields["state"] == nil || fields["updated_at"] == nil || fields["occurrences"] == nil || fields["appended_bytes"] == nil {
				t.Fatalf("unexpected acknowledgment: %s", encoded)
			}
		})
	}
}

func TestIssueReadCompactOptions(t *testing.T) {
	body := "intro\n" + strings.Repeat("occurrence 界\n", 20000)
	issue := model.Issue{ID: 1, Title: "large", Body: body, Kind: model.IssueCI, State: model.IssueOpen, Occurrences: 20000}
	for _, tool := range []string{"issue_get", "issue_get_many"} {
		t.Run(tool, func(t *testing.T) {
			reader := &bulkIssueReader{issues: map[int64]model.Issue{1: issue}}
			service := &API{issues: reader}
			call := func(options map[string]any) any {
				t.Helper()
				if tool == "issue_get" {
					options["id"] = 1
				} else {
					options["ids"] = []int64{1, 999}
				}
				raw, _ := json.Marshal(options)
				value, err := service.CallTool(t.Context(), api.Actor{Identity: "reader"}, tool, raw)
				if err != nil {
					t.Fatal(err)
				}
				if tool == "issue_get_many" {
					response := value.(api.IssueGetManyResponse)
					if len(response.Results) != 2 || response.Results[1].ID != 999 || response.Results[1].Error != "not_found" || response.Results[0].Error != "" {
						t.Fatalf("bad batch: %#v", response)
					}
					value = response.Results[0].Issue
				}
				return value
			}
			for _, options := range []map[string]any{
				{"fields": []string{"id", "title", "state", "updated_at"}},
				{"fields": []string{"id", "title", "state", "updated_at"}, "body_limit": 5},
			} {
				got := call(options).(map[string]any)
				if len(got) != 4 || got["id"] != issue.ID || got["title"] != issue.Title || got["state"] != string(issue.State) || got["updated_at"] == nil {
					t.Fatalf("projection: %#v", got)
				}
			}
			for _, limit := range []int{0, 1, 17} {
				got := call(map[string]any{"body_limit": limit}).(api.IssueResponse)
				if got.Body != limitIssueBody(body, limit) || got.Occurrences != int(issue.Occurrences) || !utf8.ValidString(got.Body) {
					t.Fatalf("body_limit %d: %#v", limit, got)
				}
			}
			got := call(map[string]any{"fields": []string{"body", "occurrences", "repo_id"}, "body_limit": 0}).(map[string]any)
			if len(got) != 3 || got["body"] != fmt.Sprintf("\n[truncated %d bytes]", len(body)) || got["occurrences"] != 20000 || got["repo_id"] != int64(0) {
				t.Fatalf("explicit zero/marker/count: %#v", got)
			}
			// The stored body and count never change on reads.
			if reader.issues[1] != issue {
				t.Fatal("compact read mutated issue")
			}
		})
	}
}

func TestIssueBodyLimitUTF8AndBoundary(t *testing.T) {
	for _, test := range []struct {
		body  string
		limit int
		want  string
	}{
		{"", 0, ""}, {"abc", 3, "abc"}, {"abc", 4, "abc"},
		{"abc", 0, "\n[truncated 3 bytes]"}, {"abc", 2, "ab\n[truncated 1 bytes]"},
		{"a界z", 2, "a\n[truncated 4 bytes]"}, {"a界z", 4, "a界\n[truncated 1 bytes]"},
	} {
		if got := limitIssueBody(test.body, test.limit); got != test.want || !utf8.ValidString(got) {
			t.Errorf("limit(%q, %d) = %q, want %q", test.body, test.limit, got, test.want)
		}
	}
}

func TestIssueReadOptionsRejectInvalidBeforeReading(t *testing.T) {
	for _, tool := range []string{"issue_get", "issue_get_many"} {
		for _, options := range []string{
			`"fields":[]`, `"fields":["Body"]`, `"fields":["id","id"]`, `"fields":["CIWorkSequence"]`,
			`"fields":[1]`, `"fields":"id"`, `"body_limit":-1`, `"body_limit":1.5`, `"body_limit":"1"`,
		} {
			reader := &bulkIssueReader{}
			id := `"id":1`
			if tool == "issue_get_many" {
				id = `"ids":[1]`
			}
			_, err := (&API{issues: reader}).CallTool(t.Context(), api.Actor{Identity: "reader"}, tool, json.RawMessage("{"+id+","+options+"}"))
			if !errors.Is(err, ErrInvalidInput) || len(reader.calls) != 0 {
				t.Errorf("%s %s: %v, reads %v", tool, options, err, reader.calls)
			}
		}
	}
}

func TestIssueCompactDefaultAndBudget(t *testing.T) {
	reader := &bulkIssueReader{issues: map[int64]model.Issue{
		1: {ID: 1, Body: strings.Repeat("x", api.IssueGetManyMaximumBytes+1)},
		2: {ID: 2, Body: strings.Repeat("\"\n<界", 100000)},
	}}
	service := &API{issues: reader}
	actor := api.Actor{Identity: "reader"}
	value, err := service.CallTool(t.Context(), actor, "issue_get", json.RawMessage(`{"id":1}`))
	if err != nil || value.(api.IssueResponse).Body != reader.issues[1].Body {
		t.Fatalf("default full read changed: %v", err)
	}
	for _, options := range []string{
		``, `,"fields":["body"]`, `,"body_limit":4`, `,"fields":["id","title"]`,
	} {
		value, err := service.CallTool(t.Context(), actor, "issue_get_many", json.RawMessage(`{"ids":[1,2]`+options+`}`))
		if err != nil {
			t.Fatal(err)
		}
		response := value.(api.IssueGetManyResponse)
		for _, row := range response.Results {
			if options == "" || options == `,"fields":["body"]` {
				if row.Error != "response_limit" || row.Issue != nil {
					t.Fatalf("lost budget: %#v", row)
				}
			} else if row.Error != "" || row.Issue == nil {
				t.Fatalf("compact read not admitted: %#v", row)
			}
		}
		encoded, _ := json.Marshal(response)
		if len(encoded) > api.IssueGetManyMaximumBytes {
			t.Fatal("encoded budget exceeded")
		}
	}
}

// A summary must not load the full retained timing record even when the reader
// provides one, and a timing failure must not prevent a status-only read.
type noSummaryTiming struct{ RunHistory }

func (noSummaryTiming) RunTiming(context.Context, string) (model.RunTiming, error) {
	return model.RunTiming{}, errors.New("summary loaded timing")
}

func TestRunGetSummary(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := t.Context()
	run, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "compact-run", Repository: fixture.repo, Branch: "feature/compact",
		SHA: strings.Repeat("a", 40), Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Truncate(time.Second)
	ended := started.Add(time.Second)
	for i, name := range []string{"setup", "unit"} {
		status := model.StepPassed
		if i == 1 {
			status = model.StepFailed
		}
		_, err := fixture.store.PutStepResult(ctx, model.StepResult{
			RunID: run.ID, Burn: "test", Step: name, Ordinal: i, Status: status, DeclaredSize: "M",
			StartedAt: &started, FinishedAt: &ended,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = fixture.store.FinishRun(ctx, run.ID, model.RunResult{
		Status: model.RunFailed, Phase: "testing", FailedBurn: "test", FailedStep: "unit", Error: "exit status 1",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := fixture.api(t)
	for _, id := range []string{run.ID, run.ID[:8]} {
		value, err := service.CallTool(ctx, api.Actor{Identity: "reader"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q,"summary":true}`, id)))
		if err != nil {
			t.Fatal(err)
		}
		got := value.(RunSummaryResponse)
		if got.ID != run.ID || got.Status != model.RunFailed || got.Phase != "testing" || got.FailedBurn != "test" || got.FailedStep != "unit" || got.Error != "exit status 1" {
			t.Fatalf("run summary: %#v", got)
		}
		if len(got.Steps) != 2 || got.Steps[0].Name != "setup" || got.Steps[1].Name != "unit" || got.Steps[1].Burn != "test" || got.Steps[1].Status != model.StepFailed || !got.Steps[1].StartedAt.Equal(started) || !got.Steps[1].FinishedAt.Equal(ended) {
			t.Fatalf("step summaries: %#v", got.Steps)
		}
		encoded, _ := json.Marshal(got)
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != 7 || len(fields["steps"].([]any)[0].(map[string]any)) != 5 || len(encoded) > 1024 {
			t.Fatalf("summary includes extra data: %s", encoded)
		}
	}
	for _, suffix := range []string{"", `,"summary":false`} {
		value, err := service.CallTool(ctx, api.Actor{Identity: "reader"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q%s}`, run.ID, suffix)))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := value.(RunDetailResponse); !ok {
			t.Fatalf("default shape changed: %T", value)
		}
	}
	service.history = noSummaryTiming{service.history}
	if _, err := service.CallTool(ctx, api.Actor{Identity: "reader"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q,"summary":true}`, run.ID))); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CallTool(ctx, api.Actor{Identity: "reader"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q}`, run.ID))); err == nil {
		t.Fatal("test timing reader was not exercised")
	}
}

func TestRunGetSummaryEmptySteps(t *testing.T) {
	got := summarizeRun(model.Run{ID: "queued", Status: model.RunQueued}, nil)
	encoded, _ := json.Marshal(got)
	if !strings.Contains(string(encoded), `"steps":[]`) {
		t.Fatalf("empty steps: %s", encoded)
	}
	got = summarizeRun(model.Run{}, []model.StepResult{{Step: "running", Status: model.StepRunning}})
	encoded, _ = json.Marshal(got.Steps[0])
	if !strings.Contains(string(encoded), `"started_at":null,"finished_at":null`) {
		t.Fatalf("unknown times: %s", encoded)
	}
}

func TestCompactReadsRetainScope(t *testing.T) {
	fixture := newScopeFixture(t)
	hiddenIssue := fixture.issues["codeberg"][0].ID
	hiddenRun := fixture.runs["codeberg"][0].ID
	for _, options := range []map[string]any{{"fields": []string{"id", "body"}}, {"body_limit": 0}} {
		options["id"] = hiddenIssue
		if _, err := fixture.call(scopedActor, "issue_get", options); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("hidden compact issue: %v", err)
		}
		delete(options, "id")
		options["ids"] = []int64{hiddenIssue, fixture.issues["github"][0].ID}
		value, err := fixture.call(scopedActor, "issue_get_many", options)
		if err != nil {
			t.Fatal(err)
		}
		rows := value.(api.IssueGetManyResponse).Results
		if rows[0].Error != "not_found" || rows[0].Issue != nil || rows[1].Issue == nil {
			t.Fatalf("compact batch scope: %#v", rows)
		}
	}
	for _, id := range []string{hiddenRun, hiddenRun[:8]} {
		_, err := fixture.call(scopedActor, "run_get", map[string]any{"id": id, "summary": true})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("hidden summary: %v", err)
		}
	}
	value, err := fixture.call(scopedActor, "run_get", map[string]any{"id": fixture.runs["github"][0].ID, "summary": true})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if strings.Contains(string(encoded), "Queue") || strings.Contains(string(encoded), "Sequence") {
		t.Fatal("summary leaked global sequence")
	}
}
