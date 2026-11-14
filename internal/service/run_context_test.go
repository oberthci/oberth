package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func runContextJSON(t *testing.T, value any) map[string]any {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRunRepositoryContextSurfaces(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	old, err := storetest.EnqueueRun(f.store, ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/context", SHA: strings.Repeat("a", 40), Actor: "reader", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	newer, err := storetest.EnqueueRun(f.store, ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: old.Ref, SHA: strings.Repeat("b", 40), Actor: "reader", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	reader := f.api(t)
	reader.schedulingObserver = schedulingObserverFunc(func(_ context.Context, _ model.Run) (*model.ExecutionObservation, error) {
		t.Fatal("context read polled execution")
		return nil, nil
	})
	// snake: HTTP rows carry snake_case wire names; summary: qualified repo.
	assertNames := func(t *testing.T, row map[string]any, snake, summary bool) {
		t.Helper()
		repo, ref, sha, position := "Repo", "Ref", "SHA", "QueuePosition"
		expectedRepo := f.repo.Name
		if snake || summary {
			repo, ref, sha, position = "repo", "ref", "sha", "queue_position"
		}
		if summary {
			expectedRepo = "codeberg/acme/oberth"
		}
		if row[repo] != expectedRepo || row[ref] != old.Ref || row[sha] != old.SHA || row[position] != nil || row["superseded"] != true {
			t.Errorf("missing run context: %#v", row)
		}
		for _, retired := range []string{"size", "declared_resources"} {
			if _, exists := row[retired]; exists {
				t.Errorf("retired admission field %s still exposed: %#v", retired, row)
			}
		}
	}
	assert := func(t *testing.T, row map[string]any, summary bool) {
		t.Helper()
		assertNames(t, row, false, summary)
	}
	for _, view := range []string{"all"} {
		t.Run("MCP/"+view, func(t *testing.T) {
			value, err := reader.CallTool(ctx, api.Actor{Identity: "reader"}, "run_list", json.RawMessage(fmt.Sprintf(`{"view":%q}`, view)))
			if err != nil {
				t.Fatal(err)
			}
			rows := runContextJSON(t, value)["runs"].([]any)
			if len(rows) != 2 {
				t.Fatalf("run count=%d, want 2", len(rows))
			}
			oldIndex, newIndex := 1, 0
			oldRow, newRow := rows[oldIndex].(map[string]any), rows[newIndex].(map[string]any)
			if oldRow["ID"] != old.ID || newRow["ID"] != newer.ID || newRow["QueuePosition"] != nil {
				t.Fatalf("unexpected %s queue order: %#v", view, rows)
			}
			assert(t, oldRow, false)
			if newRow["superseded"] != false {
				t.Error("newest run is superseded")
			}
		})
	}
	t.Run("summary", func(t *testing.T) {
		value, err := reader.CallTool(ctx, api.Actor{Identity: "reader"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q,"summary":true}`, old.ID[:8])))
		if err != nil {
			t.Fatal(err)
		}
		assert(t, runContextJSON(t, value), true)
	})
	server, err := api.New(runSurfaceAuth{}, reader, reader, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/runs", "/api/runs/" + old.ID} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("HTTP %d %s", response.Code, response.Body)
			}
			var value any
			if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			if rows, ok := value.(map[string]any)["runs"].([]any); ok {
				if len(rows) != 2 {
					t.Fatalf("run count=%d, want 2", len(rows))
				}
				oldIndex, newIndex := 1, 0
				oldRow, newRow := rows[oldIndex].(map[string]any), rows[newIndex].(map[string]any)
				if oldRow["id"] != old.ID || newRow["id"] != newer.ID || newRow["queue_position"] != nil || oldRow["ID"] != nil {
					t.Fatalf("unexpected %s queue order: %#v", path, rows)
				}
				assertNames(t, oldRow, true, false)
				if newRow["superseded"] != false {
					t.Error("newest run is superseded")
				}
			} else {
				assertNames(t, value.(map[string]any)["run"].(map[string]any), true, false)
			}
		})
	}
	t.Run("superseded-filter", func(t *testing.T) {
		value, err := reader.CallTool(ctx, api.Actor{Identity: "admin", Admin: true}, "run_list", json.RawMessage(`{"superseded":true}`))
		if err != nil {
			t.Fatal(err)
		}
		rows := runContextJSON(t, value)["runs"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["ID"] != old.ID {
			t.Fatalf("filtered runs: %#v", rows)
		}
		for _, id := range []string{old.ID, newer.ID} {
			run, err := f.store.Run(ctx, id)
			if err != nil || (id == old.ID && run.Status != model.RunInterrupted) || (id == newer.ID && run.Status != model.RunQueued) {
				t.Fatalf("read mutated run: %+v %v", run, err)
			}
		}
	})
}

func TestRunRepositoryContextUnknownAndScope(t *testing.T) {
	f := newScopeFixture(t)
	old, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.visible.ID, RefKind: model.RefBranch, Ref: "feature/context", SHA: strings.Repeat("a", 40), Actor: "reader", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.visible.ID, RefKind: model.RefBranch, Ref: old.Ref, SHA: strings.Repeat("b", 40), Actor: "reader", Trigger: "push"}); err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"all", "history"} {
		value, err := f.call(scopedActor, "run_list", map[string]any{"superseded": true, "view": view})
		f.assertNoLeak(t, "run context filter", value, err)
		if err != nil {
			t.Fatal(err)
		}
		rows := runContextJSON(t, value)["runs"].([]any)
		if len(rows) != 1 {
			t.Fatalf("filtered scoped rows: %#v", rows)
		}
		row := rows[0].(map[string]any)
		if row["Repo"] != f.visible.Name || row["superseded"] != true {
			t.Fatalf("scoped row: %#v", row)
		}
		if _, exists := row["QueuePosition"]; exists {
			t.Fatal("scoped queue position leaked")
		}
	}
	value, err := f.call(scopedActor, "run_get", map[string]any{"id": old.ID, "summary": true})
	f.assertNoLeak(t, "run context summary", value, err)
	if err != nil {
		t.Fatal(err)
	}
	row := runContextJSON(t, value)
	if row["repo"] != "github/oberthci/oberth" || row["ref"] != old.Ref || row["sha"] != old.SHA || row["superseded"] != true {
		t.Fatalf("scoped summary: %#v", row)
	}
	if _, exists := row["queue_position"]; exists {
		t.Fatal("scoped summary queue position leaked")
	}
}

func TestRunRepositoryContextHTTPFilter(t *testing.T) {
	f := newControlFixture(t)
	// A newer ordinary push supersedes the older run in the same lane.
	reader := f.api(t)
	// A nonmatching run remains ahead globally even when the filter hides it.
	if _, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/other", SHA: strings.Repeat("c", 40), Actor: "reader", Trigger: "push"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	var runs []model.Run
	for i := 0; i < 2; i++ {
		run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/context", SHA: strings.Repeat(string(rune('a'+i)), 40), Actor: "reader", Trigger: "push"})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run.Run)
		if i == 0 {
			if claimed, err := f.store.ClaimNextRun(t.Context()); err != nil || claimed.ID != run.ID {
				t.Fatalf("claim context fixture: %+v %v", claimed, err)
			}
		}
	}
	server, err := api.New(runSurfaceAuth{}, reader, reader, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query         string
		status, count int
	}{
		{"?superseded=true", 200, 1},
		{"?superseded=true&repo=" + f.repo.Name + "&ref=feature/context", 200, 1},
		{"?superseded=true&ref=other", 200, 0},
		{"?superseded=true&view=history", 200, 1},
		{"?superseded=true&latest_per_ref=1", 200, 0},
		{"?superseded=false", 200, 3},
		{"?superseded=1", 400, 0},
		{"?superseded=invalid", 400, 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/runs"+tc.query, nil)
			request.Header.Set("Authorization", "Bearer token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("HTTP %d %s", response.Code, response.Body)
			}
			if tc.status != 200 {
				return
			}
			var page struct {
				Runs []map[string]any `json:"runs"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			rows := page.Runs
			if len(rows) != tc.count {
				t.Fatalf("filtered rows: %s", response.Body)
			}
			if tc.count == 1 && (rows[0]["id"] != runs[0].ID || rows[0]["superseded"] != true || rows[0]["queue_position"] != nil) {
				t.Fatalf("wrong filtered context: %#v", rows[0])
			}
		})
	}
}
