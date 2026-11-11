package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type coverageReaderStub struct {
	calls int
	ids   []string
}

type anonymousResourceHistory struct {
	RunResolver
	RunHistory
	RepositoryReader
	values map[string]model.Run
}

func (h *anonymousResourceHistory) ListUpstreams(context.Context) ([]model.Upstream, error) {
	return []model.Upstream{{ID: 1, Name: "repo-01"}, {ID: 2, Name: "repo-02"}}, nil
}
func (h *anonymousResourceHistory) ListRepositories(context.Context) ([]model.Repository, error) {
	return []model.Repository{{ID: 1, Name: "repo-01", UpstreamID: 1}, {ID: 2, Name: "repo-02", UpstreamID: 2}}, nil
}
func (h *anonymousResourceHistory) Repository(ctx context.Context, id int64) (model.Repository, error) {
	repos, _ := h.ListRepositories(ctx)
	for _, r := range repos {
		if r.ID == id {
			return r, nil
		}
	}
	return model.Repository{}, store.ErrNotFound
}
func (h *anonymousResourceHistory) RepositoryByName(ctx context.Context, name string) (model.Repository, error) {
	repos, _ := h.ListRepositories(ctx)
	for _, r := range repos {
		if r.Name == name {
			return r, nil
		}
	}
	return model.Repository{}, store.ErrNotFound
}
func (h *anonymousResourceHistory) Run(_ context.Context, id string) (model.Run, error) {
	r, ok := h.values[id]
	if !ok {
		return model.Run{}, store.ErrNotFound
	}
	return r, nil
}
func (h *anonymousResourceHistory) ResolveRunID(ctx context.Context, id string) (model.Run, error) {
	return h.Run(ctx, id)
}
func (h *anonymousResourceHistory) StepResults(context.Context, string) ([]model.StepResult, error) {
	return []model.StepResult{}, nil
}
func TestResourceCoverageScopesBeforeReadingMessageMetadata(t *testing.T) {
	reader := &coverageReaderStub{}
	visible, hidden := strings.Repeat("a", 32), strings.Repeat("b", 32)
	history := &anonymousResourceHistory{values: map[string]model.Run{visible: {ID: visible, RepoID: 1, Status: model.RunPassed}, hidden: {ID: hidden, RepoID: 2, Status: model.RunPassed}}}
	service, err := NewAPI(APIConfig{Runs: history, History: history, Repositories: history, ResourceCoverages: reader})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Shutdown(t.Context()) }()
	actor := api.Actor{Identity: "repo-01", Scope: []string{"repo-01"}}
	raw, _ := json.Marshal(map[string]any{"id": hidden, "summary": false})
	if _, err := service.CallTool(t.Context(), actor, "run_get", raw); err == nil || reader.calls != 0 {
		t.Fatal("hidden run reached resource metadata")
	}
	raw, _ = json.Marshal(map[string]any{"id": visible, "summary": false})
	if _, err := service.CallTool(t.Context(), actor, "run_get", raw); err != nil {
		t.Fatal(err)
	}
	for _, id := range reader.ids {
		if id != visible {
			t.Fatal("out-of-scope resource metadata was read")
		}
	}
}

func (r *coverageReaderStub) ResourceCoverages(_ context.Context, ids []string) (map[string]model.ResourceCoverage, error) {
	r.calls++
	r.ids = append(r.ids, ids...)
	out := map[string]model.ResourceCoverage{}
	for _, id := range ids {
		out[id] = model.ResourceCoverage{Declared: 1, Undeclared: 2}
	}
	return out, nil
}
func TestResourceMessageUsesSeparateFieldAndOneBoundedBatch(t *testing.T) {
	reader := &coverageReaderStub{}
	s := &API{resourceCoverages: reader}
	ids := []string{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	runs := []model.Run{{ID: ids[0], DisplayReason: "running"}, {ID: ids[1], DisplayReason: "waiting"}}
	s.applyResourceMessages(t.Context(), runs)
	if reader.calls != 1 || len(reader.ids) != 2 {
		t.Fatal("not batched")
	}
	for i, run := range runs {
		if !strings.Contains(run.ResourceMessage, "2 steps") || !strings.Contains(run.ResourceMessage, "oberth run resources --run "+run.ID) {
			t.Fatal(run.ResourceMessage)
		}
		if run.DisplayReason != []string{"running", "waiting"}[i] {
			t.Fatal("resource notice changed the display reason")
		}
	}
}

func TestResourceMessagePreservesPresentationVocabulary(t *testing.T) {
	s := &API{resourceCoverages: &coverageReaderStub{}}
	for _, reason := range []string{"Priority normal: ordinary run.", "", "server restart, retry needed", "Execution deadline exceeded."} {
		runs := []model.Run{{ID: strings.Repeat("a", 32), DisplayReason: reason, Error: "step-abc12345"}}
		s.applyResourceMessages(t.Context(), runs)
		s.applyResourceMessages(t.Context(), runs)
		if runs[0].DisplayReason != reason || runs[0].Error != "step-abc12345" || strings.Count(runs[0].ResourceMessage, "2 steps") != 1 {
			t.Fatalf("presentation vocabulary changed: %+v", runs[0])
		}
		b, err := json.Marshal(runs[0])
		var fields map[string]any
		if err != nil || json.Unmarshal(b, &fields) != nil || fields["display_reason"] != reason || fields["resource_message"] != runs[0].ResourceMessage {
			t.Fatal("resource notice must have its own wire field")
		}
	}
}
func TestResourceMessageKeepsDetailedDeadlineFailure(t *testing.T) {
	raw := model.ExecutionDeadlineMessage + " Step step-12345678/step-abc12345: deadline limit 60s; sampled memory peak unknown. Inspect oberth run resources --run " + strings.Repeat("a", 32) + " --include-failed; change deadlines in git."
	got := presentationRun(model.Run{ID: strings.Repeat("a", 32), Status: model.RunFailed, Error: raw})
	if got.DisplayStatus != "failed" || got.DisplayReason != "infrastructure error: deadline exceeded" || got.Error != raw {
		t.Fatalf("failure guidance lost: %+v", got)
	}
}
