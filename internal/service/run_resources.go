package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type RunResourceReader interface {
	RunResources(context.Context, int64, store.RunResourceFilter) (store.RunResourceHistory, error)
}
type ResourceDeclarationReader interface {
	ResourceDeclarations(context.Context, WorkflowRequest) ([]measured.Declaration, error)
}

func (s *API) readRunResources(ctx context.Context, raw json.RawMessage) (any, error) {
	var a struct {
		Repo          string `json:"repo"`
		Run           string `json:"run"`
		Stage         string `json:"burn"`
		Step          string `json:"step"`
		Ref           string `json:"ref"`
		Last          int    `json:"last"`
		IncludeFailed bool   `json:"include_failed"`
		Format        string `json:"format"`
	}
	if e := decodeTool(raw, &a); e != nil {
		return nil, e
	}
	if a.Last == 0 {
		a.Last = measured.DefaultRuns
	}
	if a.Format == "" {
		a.Format = "json"
	}
	if a.Last < 1 || a.Last > measured.MaxRuns || len(a.Stage) > 253 || len(a.Step) > 253 || len(a.Ref) > 1024 || a.Format != "json" && a.Format != "yaml" || a.Repo == "" && a.Run == "" || a.Run != "" && a.Ref != "" {
		return nil, refuse(ErrInvalidInput, "run_resources: repo or run required; last 1..100; format json or yaml; run and ref are exclusive")
	}
	if s.repositories == nil || s.history == nil || s.runResources == nil {
		return nil, refuse(ErrUnavailable, "resource history")
	}
	var repo model.Repository
	var exact model.Run
	var e error
	if a.Run != "" {
		exact, e = s.history.ResolveRunID(ctx, a.Run)
		if e != nil {
			return nil, e
		}
		repo, e = s.repositories.Repository(ctx, exact.RepoID)
		if e != nil {
			return nil, e
		}
		if a.Repo != "" {
			selected, e := s.repositories.RepositoryByName(ctx, a.Repo)
			if e != nil {
				return nil, e
			}
			if selected.ID != repo.ID {
				return nil, refuse(store.ErrNotFound, "run resources")
			}
		}
	} else {
		repo, e = s.repositories.RepositoryByName(ctx, a.Repo)
		if e != nil {
			return nil, e
		}
	}
	includePromotions := a.Ref == "" && exact.ID == ""
	if a.Ref == "" {
		a.Ref = repo.DefaultBranch
	}
	filter := store.RunResourceFilter{Ref: a.Ref, Runs: a.Last, IncludeFailed: a.IncludeFailed, IncludePromotions: includePromotions}
	if exact.ID != "" {
		filter.RunID = exact.ID
		filter.Ref = "any"
		filter.Runs = 1
	}
	// Raw history access occurs only after the scoped repo/run wrappers authorize.
	history, e := s.runResources.RunResources(ctx, repo.ID, filter)
	if e != nil {
		return nil, e
	}
	r := measured.Report{Format: a.Format, Repository: repo.Name, Ref: a.Ref, Run: exact.ID, RequestedRuns: filter.Runs, SelectedRuns: len(history.Runs), IncludeFailed: a.IncludeFailed, IncludePromotions: includePromotions, Truncated: history.Truncated, DeclarationsStatus: "unavailable", Retention: measured.Retention, Semantics: measured.Semantics, Steps: []measured.Step{}}
	var declarations []measured.Declaration
	if len(history.Runs) > 0 {
		newest := history.Runs[0]
		r.NewestSHA = newest.TestedSHA
		if r.NewestSHA == "" {
			r.NewestSHA = newest.SHA
		}
		if s.resourceDeclarations != nil {
			resolver, ok := s.runs.(interface {
				Upstream(context.Context, int64) (model.Upstream, error)
			})
			if ok {
				up, e := resolver.Upstream(ctx, repo.UpstreamID)
				if e != nil {
					return nil, e
				}
				declarations, e = s.resourceDeclarations.ResourceDeclarations(ctx, WorkflowRequest{Run: newest, Repository: repo, UpstreamName: up.Name, UpstreamOrg: up.Org()})
				if e == nil {
					r.DeclarationsStatus = "newest-selected-sha"
				} else {
					r.DeclarationsStatus = "unavailable-at-newest-selected-sha"
				}
			}
		}
	}
	// Map legacy fragment hashes only through an exact expanded template in
	// the current declaration lock, never by a hash-free name alone. Unknown/conflicting owners remain explicit, not guessed.
	owners := map[string]string{}
	for _, d := range declarations {
		k := measured.NormalizeTemplate(d.Stage) + "\x00" + d.ResolvedTemplate
		if previous, ok := owners[k]; ok && previous != d.OwnerRepository {
			owners[k] = ""
		} else {
			owners[k] = d.OwnerRepository
		}
	}
	for i := range history.Observations {
		o := &history.Observations[i]
		if o.Owner == "" {
			if strings.HasPrefix(o.Template, "frag-") {
				o.Owner = owners[measured.NormalizeTemplate(o.Usage.Stage)+"\x00"+o.Template]
			} else {
				o.Owner = repo.Name
			}
		}
	}
	for _, step := range measured.Build(history.Observations, declarations) {
		if a.Stage != "" && step.Stage != measured.NormalizeTemplate(a.Stage) {
			continue
		}
		if a.Step != "" && step.Template != measured.NormalizeTemplate(a.Step) {
			continue
		}
		if r.Truncated {
			step.Suggested = nil
			step.NoSuggestion = append(step.NoSuggestion, "history row limit reached; narrow the run selection")
		}
		r.Steps = append(r.Steps, step)
	}
	if e := ctx.Err(); e != nil {
		return nil, fmt.Errorf("resource report: %w", e)
	}
	return r, nil
}
