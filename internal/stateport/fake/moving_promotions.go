package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"slices"
)

func (f *Runs) PreparePromotion(ctx context.Context, a stateport.Action, p model.Promotion) error {
	return f.movingChange(ctx, "PreparePromotion", a.Events, func(m *runMemory) error {
		if p.ID == "" || p.Repo == "" || p.Actor != a.Actor || p.Caller.Identity != a.Actor || p.ResultSHA == "" {
			return runProblem(publicerr.InvalidInput)
		}
		if _, ok := m.promotions[p.ID]; ok {
			return runProblem(publicerr.Conflict)
		}
		p.Revision = 1
		p.Status = model.PromotionPending
		m.promotions[p.ID] = p.Clone()
		m.record(a.Actor, "promotion.prepare", p.ID, a.Reason)
		return nil
	})
}
func (f *Runs) Promotion(ctx context.Context, id string) (model.Promotion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "Promotion", false); err != nil {
		return model.Promotion{}, err
	}
	p, ok := f.data.promotions[id]
	if !ok {
		return p, runProblem(publicerr.NotFound)
	}
	return p.Clone(), nil
}
func (f *Runs) PromotionForRun(ctx context.Context, id string) (model.Promotion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "PromotionForRun", false); err != nil {
		return model.Promotion{}, err
	}
	for _, p := range f.data.promotions {
		if p.RunID == id {
			return p.Clone(), nil
		}
	}
	return model.Promotion{}, runProblem(publicerr.NotFound)
}
func (f *Runs) Promotions(ctx context.Context, q stateport.RecordFilter) ([]model.Promotion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "Promotions", false); err != nil {
		return nil, err
	}
	out := []model.Promotion{}
	for _, p := range f.data.promotions {
		if visibleRun(q.Repositories, p.Repo) && (q.Before == "" || p.ID < q.Before) && (q.Status == "" || string(p.Status) == q.Status) && (q.SourceSHA == "" || p.SourceSHA == q.SourceSHA) {
			out = append(out, p.Clone())
		}
	}
	slices.SortFunc(out, func(a, b model.Promotion) int {
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
func (m *runMemory) finishPromotion(a stateport.Action, q stateport.PromotionUpdate) error {
	p, ok := m.promotions[q.ID]
	if !ok {
		return runProblem(publicerr.NotFound)
	}
	if p.Revision != q.Revision || q.RunID != "" && p.RunID != "" && q.RunID != p.RunID {
		return runProblem(publicerr.Conflict)
	}
	if !q.Status.Valid() {
		return runProblem(publicerr.InvalidInput)
	}
	p = p.Clone()
	p.Revision++
	p.Status = q.Status
	p.Reason = q.Reason
	p.Detail = q.Detail
	if q.RunID != "" {
		p.RunID = q.RunID
	}
	m.promotions[p.ID] = p
	m.record(a.Actor, "promotion.finish", p.ID, a.Reason)
	return nil
}
func (f *Runs) FinishPromotion(ctx context.Context, a stateport.Action, q stateport.PromotionUpdate) error {
	return f.movingChange(ctx, "FinishPromotion", a.Events, func(m *runMemory) error {
		if err := m.finishPromotion(a, q); err != nil {
			return err
		}
		if q.Publication != nil {
			return m.publication(a, q.Publication.Publication, m.runs[q.RunID])
		}
		return nil
	})
}
func (m *runMemory) linkPromotion(a stateport.Acceptance, r stateport.Run) error {
	if a.PromotionID == "" {
		return nil
	}
	p, ok := m.promotions[a.PromotionID]
	if !ok || p.Repo != r.Repo || p.ResultSHA != r.ObjectSHA || r.CommitSHA == nil || *r.CommitSHA != p.ResultSHA || r.Origin.Trigger != model.TriggerPromotion || r.Status != stepmodel.RunRunning {
		return runProblem(publicerr.InvalidInput)
	}
	if p.RunID != "" && p.RunID != r.ID {
		return runProblem(publicerr.Conflict)
	}
	p = p.Clone()
	p.RunID = r.ID
	p.Revision++
	m.promotions[p.ID] = p
	return nil
}
