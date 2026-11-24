package service

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Service) allow(ctx context.Context, a actor.Actor, action string) (actor.Actor, error) {
	if err := ctx.Err(); err != nil {
		return actor.Actor{}, err
	}
	if s.ctx.Err() != nil {
		return actor.Actor{}, refuse(unavailable)
	}
	if !authenticated(a) {
		return actor.Actor{}, refuse(forbidden)
	}
	current, err := s.c.Authority.Current(ctx, a)
	if err != nil {
		return actor.Actor{}, err
	}
	if current.Identity != a.Identity || !authenticated(current) {
		return actor.Actor{}, refuse(forbidden)
	}
	need, ok := apicontract.Required(action)
	if !ok {
		return actor.Actor{}, bad("action")
	}
	if !need.Allows(current) {
		if need == apicontract.Admin {
			return actor.Actor{}, refuse(installation)
		}
		return actor.Actor{}, refuse(forbidden)
	}
	return current, nil
}

func authenticated(a actor.Actor) bool {
	return strings.TrimSpace(a.Identity) != "" && a.AuthMethod != ""
}
func (s *Service) resolveRepo(ctx context.Context, a actor.Actor, selector string) (stateport.Repository, error) {
	return ResolveRepository(ctx, s.c.Catalog, a, selector)
}

// ResolveRepository applies the same registered-repository scope to every transport.
func ResolveRepository(ctx context.Context, catalog CatalogStore, a actor.Actor, selector string) (stateport.Repository, error) {
	if reponame.ValidateSelector(selector) != nil {
		return stateport.Repository{}, bad("repo")
	}
	scope, err := catalog.RepositoryScope(ctx, upstreamScope(a))
	if err != nil {
		return stateport.Repository{}, err
	}
	return catalog.Repository(ctx, selector, scope)
}
func validID(id string) bool {
	if len(id) < 8 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id + strings.Repeat("0", len(id)%2))
	return err == nil
}
func validRunID(id string) bool {
	return validID(id) || nativebinding.ValidChildRunID(id)
}
func (s *Service) resolveRun(ctx context.Context, a actor.Actor, q runQuery) (stateport.Run, error) {
	scope, err := s.repositoryScope(ctx, a)
	if err != nil {
		return stateport.Run{}, err
	}
	if q.Repo != "" {
		repo, err := s.resolveRepo(ctx, a, q.Repo)
		if err != nil {
			return stateport.Run{}, runLookupError(err)
		}
		scope = []string{repo.Canonical}
	}
	if q.Trigger != "" && !q.Trigger.Valid() {
		return stateport.Run{}, bad("trigger")
	}
	r, err := s.resolveSelector(ctx, scope, q)
	if err != nil || q.Trigger == "" {
		return r, runLookupError(err)
	}
	r, err = s.triggerRun(ctx, runSelection{scope, q, r})
	return r, runLookupError(err)
}

func runLookupError(err error) error {
	if isMissing(err) {
		return refuse(runMissing)
	}
	return err
}
func (s *Service) resolveSelector(ctx context.Context, scope []string, q runQuery) (stateport.Run, error) {
	switch {
	case q.ID != "":
		if !validRunID(q.ID) {
			return stateport.Run{}, bad("id")
		}
		r, err := s.selectRunID(ctx, scope, q)
		// Hex IDs allow prefixes; native child names must match in full.
		if err == nil && r.ID != q.ID && !validID(q.ID) {
			return stateport.Run{}, refuse(missing)
		}
		return r, err
	case q.SHA != "":
		if strings.HasPrefix(q.SHA, "refs/") {
			return s.c.Runs.RunByRef(ctx, scope, q.SHA)
		}
		if !validID(q.SHA) {
			return stateport.Run{}, bad("sha")
		}
		return s.c.Runs.RunByCommit(ctx, scope, q.SHA)
	case q.Ref != "":
		if validID(q.Ref) {
			return s.c.Runs.RunByCommit(ctx, scope, q.Ref)
		}
		ref := q.Ref
		if !strings.HasPrefix(ref, "refs/") {
			ref = "refs/heads/" + ref
		}
		return s.c.Runs.RunByRef(ctx, scope, ref)
	default:
		return stateport.Run{}, bad("id/sha/ref")
	}
}

func (s *Service) selectRunID(ctx context.Context, scope []string, q runQuery) (stateport.Run, error) {
	if q.View {
		return s.c.Runs.LookupRun(ctx, scope, q.ID)
	}
	return s.c.Runs.ResolveRun(ctx, scope, q.ID)
}

type runSelection struct {
	scope []string
	q     runQuery
	r     stateport.Run
}

func (s *Service) triggerRun(ctx context.Context, input runSelection) (stateport.Run, error) {
	scope, q, r := input.scope, input.q, input.r
	if q.ID != "" {
		if r.Origin.Trigger != q.Trigger {
			return stateport.Run{}, refuse(missing)
		}
		return r, nil
	}
	f := stateport.RunFilter{Repositories: scope, Trigger: q.Trigger, Ref: r.Ref, Limit: 1}
	if q.SHA != "" && r.CommitSHA != nil {
		f.CommitSHA = *r.CommitSHA
		f.Ref = ""
	}
	rows, err := s.c.Runs.Runs(ctx, f)
	if err != nil {
		return stateport.Run{}, err
	}
	if len(rows) == 0 {
		return stateport.Run{}, refuse(missing)
	}
	return rows[0], nil
}

func (s *Service) repositoryScope(ctx context.Context, a actor.Actor) ([]string, error) {
	return s.c.Catalog.RepositoryScope(ctx, upstreamScope(a))
}
func upstreamScope(a actor.Actor) []string {
	if len(a.Scope) == 0 {
		return nil
	}
	return a.Scope
}
