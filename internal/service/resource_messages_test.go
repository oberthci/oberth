package service

import (
	"context"
	"strings"
	"testing"

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

func TestResourceMessageKeepsDetailedDeadlineFailure(t *testing.T) {
	raw := model.ExecutionDeadlineMessage + " Step step-12345678/step-abc12345: deadline limit 60s; sampled memory peak unknown. Inspect oberth run resources --run " + strings.Repeat("a", 32) + " --include-failed; change deadlines in git."
	got := presentationRun(model.Run{ID: strings.Repeat("a", 32), Status: model.RunFailed, Error: raw})
	if got.DisplayStatus != "failed" || got.DisplayReason != "infrastructure error: deadline exceeded" || got.Error != raw {
		t.Fatalf("failure guidance lost: %+v", got)
	}
}
