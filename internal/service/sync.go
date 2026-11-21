package service

import (
	"context"
	"strings"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Service) PrepareSync(ctx context.Context, c apicontract.Caller, q apicontract.SyncArgs) (apicontract.Call[apicontract.Receipt], error) {
	a, err := s.allow(ctx, c.Actor, "sync")
	if err != nil {
		return nil, err
	}
	p, err := s.syncIntent(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		if err := s.c.Publications.PreparePublication(ctx, stateport.Action{Actor: a.Identity, Reason: q.Reason}, stateport.PublicationIntent{Publication: p}); err != nil {
			return apicontract.Receipt{}, err
		}
		s.schedule(p.ID, nil)
		return apicontract.Receipt{ID: p.ID, State: reasonPubPending}, nil
	}), nil
}

func (s *Service) syncIntent(ctx context.Context, a actor.Actor, q apicontract.SyncArgs) (model.Publication, error) {
	r, err := s.resolveRepo(ctx, a, q.Repo)
	if err != nil {
		return model.Publication{}, err
	}
	if r.Archived {
		return model.Publication{}, refuse(archived)
	}
	head, err := s.syncHead(ctx, r.Canonical, q)
	if err != nil {
		return model.Publication{}, err
	}
	ref := head.Name
	rows, err := s.c.Runs.Runs(ctx, stateport.RunFilter{Repositories: []string{r.Canonical}, Ref: ref, Limit: 1})
	if err != nil {
		return model.Publication{}, err
	}
	if len(rows) > 0 && (rows[0].Release || rows[0].Origin.Trigger != model.TriggerBranch || (rows[0].Origin.HasAssistance())) {
		return model.Publication{}, refuse(syncProtected)
	}
	p := model.Publication{ID: s.c.NewID(), Repo: r.Canonical, Ref: ref, ObjectSHA: q.SHA, CommitSHA: new(q.SHA), Actor: a.Identity}
	return p, nil
}

func (s *Service) syncHead(ctx context.Context, repo string, q apicontract.SyncArgs) (gitport.Ref, error) {
	ref := q.Branch
	if ref == "" {
		return gitport.Ref{}, bad("branch")
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	if !strings.HasPrefix(ref, "refs/heads/") {
		return gitport.Ref{}, refuse(syncProtected)
	}
	defaultRef, err := s.c.Source.DefaultBranch(ctx, repo)
	if err != nil {
		return gitport.Ref{}, err
	}
	if ref == defaultRef {
		return gitport.Ref{}, refuse(syncProtected)
	}
	head, err := s.c.Git.ResolveRef(ctx, repo, ref)
	if err != nil {
		return gitport.Ref{}, err
	}
	if (gitport.Ref{Name: ref, ObjectSHA: q.SHA, CommitSHA: q.SHA}).Validate() != nil || head.ObjectSHA != q.SHA || head.CommitSHA != q.SHA {
		return gitport.Ref{}, bad("sha")
	}
	return head, nil
}
