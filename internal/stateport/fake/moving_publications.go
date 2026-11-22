package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"slices"
	"strings"
)

type publicationChange struct {
	id, operation string
	action        stateport.Action
	apply         func(*runMemory, *model.Publication) error
}

func (f *Runs) changePublication(ctx context.Context, q publicationChange) (model.Publication, error) {
	var result model.Publication
	err := f.movingChange(ctx, q.operation, q.action.Events, func(m *runMemory) error {
		p, ok := m.publications[q.id]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		p = p.Clone()
		result = p
		if err := q.apply(m, &p); err != nil {
			return err
		}
		m.publications[p.ID] = p.Clone()
		m.record(q.action.Actor, q.operation, p.ID, q.action.Reason)
		result = p.Clone()
		return nil
	})
	return result, err
}
func (f *Runs) PreparePublication(ctx context.Context, a stateport.Action, intent stateport.PublicationIntent) error {
	return f.movingChange(ctx, "PreparePublication", a.Events, func(m *runMemory) error {
		p := intent.Publication.Clone()
		if p.ID == "" || p.Actor == "" || p.Actor != a.Actor || p.Repo == "" || p.Ref == "" || invalidTagBase(p) {
			return runProblem(publicerr.InvalidInput)
		}
		if _, ok := m.publications[p.ID]; ok {
			return runProblem(publicerr.Conflict)
		}
		if p.RunID != "" {
			return m.publication(a, p, m.runs[p.RunID])
		}
		p.Status = model.PublicationPending
		p.Attempts = 0
		p.RetryAt = nil
		p.CreatedAt = f.clock()
		p.UpdatedAt = p.CreatedAt
		m.publications[p.ID] = p
		m.record(a.Actor, "publication.prepare", p.ID, a.Reason)
		return nil
	})
}
func (f *Runs) BindPublication(ctx context.Context, a stateport.Action, id, previous string) (model.Publication, error) {
	return f.changePublication(ctx, publicationChange{id, "publication.bind", a, func(_ *runMemory, p *model.Publication) error {
		if p.Status != model.PublicationPending {
			return runProblem(publicerr.Conflict)
		}
		if strings.HasPrefix(p.Ref, "refs/tags/") && previous != "" {
			return runProblem(publicerr.Conflict)
		}
		if p.PreviousSHA != nil {
			if *p.PreviousSHA == previous {
				return noRunChange{}
			}
			return runProblem(publicerr.Conflict)
		}
		p.PreviousSHA = new(previous)
		return nil
	}})
}
func (f *Runs) StartPublicationAttempt(ctx context.Context, a stateport.Action, q stateport.PublicationAttempt) (model.Publication, error) {
	return f.changePublication(ctx, publicationChange{q.ID, "publication.attempt", a, func(_ *runMemory, p *model.Publication) error {
		if p.Status != model.PublicationPending || p.Attempts != q.ExpectedAttempts || p.Attempts >= pushretry.MaxAttempts {
			return runProblem(publicerr.Conflict)
		}
		if p.Attempts > 0 && p.RetryAt == nil || p.RetryAt != nil && f.clock().Before(*p.RetryAt) {
			return runProblem(publicerr.Conflict)
		}
		p.Attempts++
		p.RetryAt = nil
		p.UpdatedAt = f.clock()
		return nil
	}})
}
func (f *Runs) SettlePublication(ctx context.Context, a stateport.Action, q stateport.PublicationResult) error {
	_, err := f.changePublication(ctx, publicationChange{q.ID, "publication.settle", a, func(m *runMemory, p *model.Publication) error {
		if p.Status != model.PublicationPending || p.Attempts != q.Attempt || q.Attempt < 1 {
			return runProblem(publicerr.Conflict)
		}
		if err := m.validateRelease(*p, q); err != nil {
			return err
		}
		p.Status = model.PublicationFailed
		p.Reason = q.Reason
		p.Detail = q.Detail
		p.RetryAt = nil
		p.UpdatedAt = f.clock()
		if q.Delivered {
			p.Status = model.PublicationDelivered
			if q.ProjectCI {
				if err := m.projectPublished(*p, f.clock()); err != nil {
					return err
				}
			}
		} else if q.Retryable && p.Attempts < pushretry.MaxAttempts {
			p.Status = model.PublicationPending
			p.RetryAt = new(p.UpdatedAt.Add(pushretry.Delay(p.Attempts)))
		}
		if q.Release != nil {
			receipt := *q.Release
			p.Release = &receipt
		}
		return nil
	}})
	return err
}
func (f *Runs) RetryPublication(ctx context.Context, a stateport.Action, q stateport.PublicationAttempt) error {
	_, err := f.changePublication(ctx, publicationChange{q.ID, "publication.retry", a, func(_ *runMemory, p *model.Publication) error {
		if p.Attempts != q.ExpectedAttempts {
			return runProblem(publicerr.Conflict)
		}
		if p.Status != model.PublicationFailed && (p.Status != model.PublicationPending || p.RetryAt == nil) {
			return runProblem(publicerr.InvalidTransition)
		}
		p.Attempts = 0
		p.Status = model.PublicationPending
		p.RetryAt = nil
		p.Reason = ""
		return nil
	}})
	return err
}
func (f *Runs) AbandonPublication(ctx context.Context, a stateport.Action, id string) error {
	_, err := f.changePublication(ctx, publicationChange{id, "publication.abandon", a, func(_ *runMemory, p *model.Publication) error {
		if p.Status != model.PublicationPending && p.Status != model.PublicationFailed {
			return runProblem(publicerr.Conflict)
		}
		p.Status = model.PublicationAbandoned
		p.RetryAt = nil
		p.Reason = a.Reason
		return nil
	}})
	return err
}
func (f *Runs) Publications(ctx context.Context, q stateport.RecordFilter) ([]model.Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "Publications", false); err != nil {
		return nil, err
	}
	out := []model.Publication{}
	for _, p := range f.data.publications {
		if visibleRun(q.Repositories, p.Repo) && (q.Before == "" || p.ID < q.Before) && (q.Status == "" || string(p.Status) == q.Status) {
			out = append(out, p.Clone())
		}
	}
	slices.SortFunc(out, func(a, b model.Publication) int {
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	return out[:min(len(out), q.Limit)], nil
}
func (f *Runs) PendingPublications(ctx context.Context, q stateport.RecordFilter) ([]model.Publication, error) {
	q.Status = string(model.PublicationPending)
	return f.Publications(ctx, q)
}

func (m *runMemory) validateRelease(p model.Publication, q stateport.PublicationResult) error {
	if q.Release == nil {
		return nil
	}
	v := *q.Release
	if !q.Delivered || v.PublicationID != p.ID || v.RunID != p.RunID || v.Tag != p.Ref || !strings.HasPrefix(p.Ref, "refs/tags/") {
		return runProblem(publicerr.InvalidInput)
	}
	r := m.runs[p.RunID]
	if !validReleaseRun(r, v.CommitSHA) || p.CommitSHA == nil || v.CommitSHA != *p.CommitSHA {
		return runProblem(publicerr.Forbidden)
	}
	if _, exists := m.publishedRelease(p.Repo, p.Ref); exists {
		return runProblem(publicerr.Conflict)
	}
	return nil
}

func validReleaseRun(r stateport.Run, sha string) bool {
	return r.Release && !r.Origin.HasAssistance() && r.Status == stepmodel.RunPassed && r.CommitSHA != nil && sha == *r.CommitSHA
}
