package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Service) visiblePublication(ctx context.Context, a actor.Actor, id string) (model.Publication, error) {
	if id == "" {
		return model.Publication{}, bad("id")
	}
	p, err := s.c.Publications.Publication(ctx, id)
	if err != nil {
		return p, err
	}
	_, err = s.resolveRepo(ctx, a, p.Repo)
	return p, err
}
func (s *Service) PreparePublicationList(ctx context.Context, c apicontract.Caller, q apicontract.PublicationListArgs) (apicontract.Call[apicontract.PublicationPage], error) {
	a, err := s.allow(ctx, c.Actor, "publication_list")
	if err != nil {
		return nil, err
	}
	if q.Status != "" && !model.PublicationStatus(q.Status).Valid() {
		return nil, bad("status")
	}
	scope, err := s.repositoryScope(ctx, a)
	if err != nil {
		return nil, err
	}
	if q.Repo != "" {
		r, err := s.resolveRepo(ctx, a, q.Repo)
		if err != nil {
			return nil, err
		}
		scope = []string{r.Canonical}
	}
	filter := stateport.RecordFilter{Repositories: scope, Before: q.Before, Status: q.Status, Limit: pageLimit(q.Limit, 50)}
	return bounded(func(ctx context.Context) (apicontract.PublicationPage, error) {
		rows, err := s.c.Publications.Publications(ctx, filter)
		page := apicontract.PublicationPage{Publications: rows}
		if page.Publications == nil {
			page.Publications = []model.Publication{}
		}
		if len(rows) == filter.Limit {
			page.NextCursor = rows[len(rows)-1].ID
		}
		return page, err
	}), nil
}
func (s *Service) PreparePublishRetry(ctx context.Context, c apicontract.Caller, q apicontract.PublishRetryArgs) (apicontract.Call[apicontract.Receipt], error) {
	a, err := s.allow(ctx, c.Actor, "publish_retry")
	if err != nil {
		return nil, err
	}
	p, err := s.visiblePublication(ctx, a, q.ID)
	if err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		release, err := s.acquire(ctx, refKey(p.Repo, p.Ref))
		if err != nil {
			return apicontract.Receipt{}, err
		}
		defer release()
		current, err := s.c.Publications.Publication(ctx, p.ID)
		if err != nil {
			return apicontract.Receipt{}, err
		}
		err = s.c.Publications.RetryPublication(ctx, stateport.Action{Actor: a.Identity, Reason: q.Reason}, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: current.Attempts})
		if err == nil {
			s.schedule(p.ID, nil)
		}
		return apicontract.Receipt{ID: p.ID, State: reasonPubPending}, err
	}), nil
}
func (s *Service) PreparePublicationAbandon(ctx context.Context, c apicontract.Caller, q apicontract.PublicationAbandonArgs) (apicontract.Call[apicontract.Receipt], error) {
	a, err := s.allow(ctx, c.Actor, "publication_abandon")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(q.Reason) == "" {
		return nil, bad("reason")
	}
	p, err := s.visiblePublication(ctx, a, q.ID)
	if err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		release, err := s.acquire(ctx, refKey(p.Repo, p.Ref))
		if err != nil {
			return apicontract.Receipt{}, err
		}
		defer release()
		err = s.c.Publications.AbandonPublication(ctx, stateport.Action{Actor: a.Identity, Reason: q.Reason}, p.ID)
		return apicontract.Receipt{ID: p.ID, State: reasonPubAbandoned}, err
	}), nil
}

// Deliver performs one durable attempt for an already authorized intent.
// Remote failures do not repeat admission, run execution or authority decisions.
func (s *Service) Deliver(ctx context.Context, id string) error {
	successor, err := s.deliverAttempt(ctx, id)
	if err != nil || successor == "" {
		return err
	}
	return s.successor(ctx, successor)
}
func (s *Service) deliverAttempt(ctx context.Context, id string) (string, error) {
	op, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = op
	p, err := s.c.Publications.Publication(ctx, id)
	if err != nil {
		return "", err
	}
	release, err := s.acquire(ctx, refKey(p.Repo, p.Ref))
	if err != nil {
		return "", err
	}
	defer release()
	p, err = s.c.Publications.Publication(ctx, id)
	if err != nil || p.Status != model.PublicationPending {
		return "", err
	}
	if p.RetryAt != nil && s.c.Now().Before(*p.RetryAt) {
		s.schedule(id, p.RetryAt)
		return "", nil
	}
	readBack := p.ReadBack || p.Attempted
	p, err = s.c.Publications.StartPublicationAttempt(ctx, stateport.Action{Actor: p.Actor}, stateport.PublicationAttempt{ID: id, ExpectedAttempts: p.Attempts})
	if err != nil {
		return "", err
	}
	p.ReadBack = readBack
	already, err := s.alreadyReleased(ctx, p)
	if err != nil || already {
		return "", s.settle(ctx, p, err)
	}
	return s.pushAttempt(ctx, p)
}

func (s *Service) alreadyReleased(ctx context.Context, p model.Publication) (bool, error) {
	if !strings.HasPrefix(p.Ref, "refs/tags/") {
		return false, nil
	}
	old, err := s.c.Publications.PublishedRelease(ctx, p.Repo, p.Ref)
	if isMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if old.ObjectSHA != p.ObjectSHA || old.CommitSHA == nil || p.CommitSHA == nil || *old.CommitSHA != *p.CommitSHA {
		return false, refuse(publicationEvidence)
	}
	return true, nil
}
func (s *Service) pushAttempt(ctx context.Context, p model.Publication) (string, error) {
	var err error
	readBack := p.ReadBack
	if p.PreviousSHA == nil {
		previous := ""
		if !strings.HasPrefix(p.Ref, "refs/tags/") {
			ref, readErr := s.remoteRef(ctx, p.Repo, p.Ref)
			if readErr != nil {
				return "", s.settle(ctx, p, readErr)
			}
			previous = ref.ObjectSHA
		}
		p, err = s.c.Publications.BindPublication(ctx, stateport.Action{Actor: p.Actor}, p.ID, previous)
		if err != nil {
			return "", err
		}
	}
	// Binding reloads durable facts; keep this invocation's recovery mode.
	p.ReadBack = readBack
	receipt, err := s.c.MovingGit.Publish(ctx, p)
	var moved *gitport.RefMovedError
	if errors.As(err, &moved) {
		if err := s.c.Publications.AbandonPublication(context.WithoutCancel(ctx), stateport.Action{Actor: p.Actor, Reason: reasonPubMoved}, p.ID); err != nil {
			return "", err
		}
		if p.PromotionID != "" {
			return p.PromotionID, nil
		}
		return "", nil
	}
	if err == nil && !confirms(receipt, p) {
		err = refuse(publicationReceipt)
	}
	return "", s.settle(ctx, p, err)
}

func (s *Service) remoteRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	v, err := s.c.MovingGit.RemoteRef(ctx, repo, ref)
	var absent *gitport.AbsentError
	var empty *gitport.EmptyRepositoryError
	if errors.As(err, &empty) {
		branch, readErr := s.c.Source.DefaultBranch(ctx, repo)
		if readErr != nil {
			return v, readErr
		}
		if ref == branch {
			return gitport.Ref{Name: ref}, nil
		}
	}
	if errors.As(err, &absent) {
		return gitport.Ref{Name: ref}, nil
	}
	return v, err
}
func confirms(r gitport.PublicationReceipt, p model.Publication) bool {
	if r.Repository != p.Repo || r.ActionReceipt != p.ID || len(r.Refs) != 1 {
		return false
	}
	v := r.Refs[0]
	return v.Name == p.Ref && v.ObjectSHA == p.ObjectSHA && (p.CommitSHA == nil || v.CommitSHA == *p.CommitSHA)
}
func transient(err error) bool {
	var unavailable *gitport.UnavailableError
	return errors.As(err, &unavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || publicerr.Classify(err).Code() == publicerr.Unavailable
}
