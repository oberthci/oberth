package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// TestServiceRunListReturnsObjectShape calls the real API.CallTool with
// "run_list" and asserts the result serializes as a JSON object with a "runs"
// key that is an array. This is the mutation-evidence test for #794/#797:
// reverting the service-layer RunListResponse wrap must make it fail.
func TestServiceRunListReturnsObjectShape(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	actor := api.Actor{Identity: "agent@host"}

	// Call with no runs in the database — the empty page must still be an
	// object with "runs": [], not "runs": null.
	value, err := fixture.api(t).CallTool(ctx, actor, "run_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal run_list result: %v", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed[0] != '{' {
		t.Fatalf("run_list result is not a JSON object: %s", trimmed[:min(80, len(trimmed))])
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("run_list result is not a JSON object: %v", err)
	}
	runsRaw, ok := parsed["runs"]
	if !ok {
		t.Fatalf("run_list result missing 'runs' key: %s", trimmed)
	}
	// Empty page must be [] not null.
	if string(runsRaw) == "null" {
		t.Fatalf("run_list empty page serialized as null, want []")
	}
	var runs []json.RawMessage
	if err := json.Unmarshal(runsRaw, &runs); err != nil {
		t.Fatalf("run_list 'runs' is not an array: %v raw=%s", err, string(runsRaw))
	}
}

// TestServiceRunListPopulatedReturnsArray verifies that run_list with actual
// runs produces a populated array under the "runs" key.
func TestServiceRunListPopulatedReturnsArray(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	actor := api.Actor{Identity: "agent@host"}

	// Enqueue a run so the list is non-empty.
	if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "shape-test", Repository: fixture.repo,
		Branch: "feature/shape", SHA: strings.Repeat("c", 40), Actor: "agent@host",
	}); err != nil {
		t.Fatal(err)
	}

	value, err := fixture.api(t).CallTool(ctx, actor, "run_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Runs []json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("run_list result is not a JSON object with runs array: %v", err)
	}
	if len(parsed.Runs) == 0 {
		t.Fatalf("run_list with one enqueued run returned empty array")
	}
}

// TestServiceRepoListReturnsObjectShape calls the real API.CallTool with
// "repo_list" and asserts the result serializes as a JSON object with a
// "repositories" key.
func TestServiceRepoListReturnsObjectShape(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	actor := api.Actor{Identity: "agent@host"}

	value, err := fixture.api(t).CallTool(ctx, actor, "repo_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal repo_list result: %v", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed[0] != '{' {
		t.Fatalf("repo_list result is not a JSON object: %s", trimmed[:min(80, len(trimmed))])
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("repo_list result is not a JSON object: %v", err)
	}
	if _, ok := parsed["repositories"]; !ok {
		t.Fatalf("repo_list result missing 'repositories' key: %s", trimmed)
	}
}

// TestServiceAllListToolsReturnObjectShape calls CallTool for every
// list-shaped tool through the real service API and asserts each result
// serializes as a JSON object (first non-space byte is '{').
func TestServiceAllListToolsReturnObjectShape(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	actor := api.Actor{Identity: "agent@host"}

	// Enqueue one run to exercise populated paths for run_list.
	if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "all-shapes", Repository: fixture.repo,
		Branch: "feature/all-shapes", SHA: strings.Repeat("d", 40), Actor: "agent@host",
	}); err != nil {
		t.Fatal(err)
	}

	// Tools requiring additional infrastructure (SecretAccessStore,
	// PromotionRepository, Health) not wired in the control fixture are
	// covered by their own package tests. The three tools below are the
	// list-shaped tools available in the minimal control fixture.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"run_list", `{}`},
		{"repo_list", `{}`},
		{"issue_list", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := fixture.api(t).CallTool(ctx, actor, tc.name, json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("%s marshal: %v", tc.name, err)
			}
			trimmed := strings.TrimSpace(string(raw))
			if len(trimmed) == 0 || trimmed[0] != '{' {
				t.Fatalf("%s result is not a JSON object: %s", tc.name, trimmed[:min(80, len(trimmed))])
			}
		})
	}
}

// TestServiceRunListNullNormalization verifies that the store layer
// returns an empty slice (not nil) when no runs match, so the service's
// RunListResponse serializes as {"runs":[]} rather than {"runs":null}.
func TestServiceRunListNullNormalization(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	actor := api.Actor{Identity: "agent@host"}

	// No runs enqueued — the empty filter must still produce [].
	value, err := fixture.api(t).Runs(ctx, actor, api.RunFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	runs, ok := value.([]model.Run)
	if !ok {
		t.Fatalf("Runs() returned %T, want []model.Run", value)
	}
	if runs == nil {
		t.Fatalf("Runs() returned nil slice, want non-nil empty slice")
	}
	raw, err := json.Marshal(api.RunListResponse{Runs: runs})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"runs":null`) {
		t.Fatalf("RunListResponse serialized as null: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"runs":[]`) {
		t.Fatalf("RunListResponse missing empty array: %s", string(raw))
	}
}
