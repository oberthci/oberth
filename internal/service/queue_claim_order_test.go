package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestHTTPAndMCPQueuedGroupsFollowClaimOrder(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	enqueue := func(ref string) model.Run {
		t.Helper()
		result, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, Ref: ref, RefKind: model.RefBranch, SHA: strings.Repeat("a", 40), Trigger: "push", Actor: "agent@host"})
		if err != nil {
			t.Fatal(err)
		}
		return result.Run
	}
	normalClaimed := enqueue("feature/claimed")
	if run, err := f.store.ClaimNextRun(ctx); err != nil || run.ID != normalClaimed.ID {
		t.Fatalf("claim: %+v, %v", run, err)
	}
	high := enqueue("rc/first")
	normal := enqueue("feature/normal")
	highLater := enqueue("rc/last")
	if _, err := f.store.SetQueuePriorityPatterns(ctx, "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	want := []string{high.ID, highLater.ID, normalClaimed.ID, normal.ID}
	reader := f.api(t)
	check := func(value any) {
		t.Helper()
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var rows []model.Run
		if err := json.Unmarshal(payload, &rows); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		if strings.Join(ids, ",") != strings.Join(want, ",") {
			t.Errorf("got %v; want scheduler claim order %v", ids, want)
		}
	}
	for _, actor := range []api.Actor{{Identity: "agent@host"}, {Identity: "scoped@host", Scope: []string{"codeberg"}}} {
		for _, view := range []string{"all"} {
			value, err := reader.CallTool(ctx, actor, "run_list", json.RawMessage(fmt.Sprintf(`{"view":%q,"limit":1}`, view)))
			if err != nil {
				t.Fatal(err)
			}
			check(value.(api.RunListResponse).Runs)
		}
	}
	server, err := api.New(runSurfaceAuth{}, reader, reader, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "&latest_per_ref=1"} {
		request := httptest.NewRequest(http.MethodGet, "/api/runs?limit=1"+query, nil)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", response.Code, response.Body)
		}
		var page struct {
			Runs json.RawMessage `json:"runs"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		check(page.Runs)
	}
}
