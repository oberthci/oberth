package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type ResourceUsageReader interface {
	StepResourceUsage(context.Context, string) ([]model.StepResourceUsage, bool, error)
	RepositoryResourceUsage(context.Context, int64, store.ResourceUsageFilter) ([]model.StepResourceUsage, bool, error)
}

func (s *API) repoUsage(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Repo  string    `json:"repo"`
		Since time.Time `json:"since"`
		Limit int       `json:"limit"`
		Burn  string    `json:"burn"`
		Step  string    `json:"step"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	if args.Repo == "" || args.Limit < 0 || args.Limit > 5000 || len(args.Burn) > 253 || len(args.Step) > 253 {
		return nil, refuse(ErrInvalidInput, "resource report bounds")
	}
	if s.repositories == nil || s.resources == nil {
		return nil, refuse(ErrUnavailable, "resource usage")
	}
	// forActor wraps this dependency. Resolve before touching raw usage storage;
	// an out-of-scope repository is indistinguishable from a missing one.
	repo, err := s.repositories.RepositoryByName(ctx, args.Repo)
	if err != nil {
		return nil, err
	}
	if args.Limit == 0 {
		args.Limit = 1000
	}
	if args.Since.IsZero() {
		args.Since = time.Now().UTC().Add(-30 * 24 * time.Hour)
	}
	rows, truncated, err := s.resources.RepositoryResourceUsage(ctx, repo.ID, store.ResourceUsageFilter{Since: args.Since, Limit: args.Limit, Burn: args.Burn, Step: args.Step})
	if err != nil {
		return nil, err
	}
	steps := model.BuildResourceReport(rows)
	stepsTruncated := len(steps) > 128
	if stepsTruncated {
		steps = steps[:128]
	}
	return model.ResourceUsageReport{ReportOnly: true, SizeClassSemantics: "observed Workflow admission labels, not numeric per-step budgets; legacy/missing/conflicting class is unknown; sampled lower bounds do not establish safe smaller requests or classes", StepsTruncated: stepsTruncated, Repository: args.Repo, Since: args.Since, Limit: args.Limit, Observations: len(rows), Truncated: truncated, PeakSemantics: "sampled lower bounds; missing is unknown; observations are container attempts, including running/killed attempts; CPU/memory/IO main scope; ephemeral/PSI pod scope; no PVC/VM disk accounting", Steps: steps}, nil
}
