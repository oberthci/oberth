package service

import (
	"context"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
	"time"
)

// heldRunPage filters live candidates before the requested page limit.
func (s *Service) heldRunPage(ctx context.Context, a actor.Actor, f stateport.RunFilter) (apicontract.RunPage, error) {
	page := apicontract.RunPage{Runs: []apicontract.RunRecord{}, ServerTime: s.c.Now()}
	want := f.Limit
	f.Limit = stateport.RunPageLimit
	for {
		rows, err := s.c.Runs.Runs(ctx, f)
		if err != nil {
			return page, err
		}
		for _, r := range rows {
			if terminal(r.Status) && r.FinishedAt != nil && s.c.Now().Sub(*r.FinishedAt) > 4*time.Hour+time.Minute {
				continue
			}
			holds, _ := s.liveHolds(ctx, r)
			if len(holds) == 0 {
				continue
			}
			record, err := s.record(ctx, a, r)
			if err != nil {
				return apicontract.RunPage{}, err
			}
			page.Runs = append(page.Runs, record)
			if len(page.Runs) == want {
				page.NextCursor = r.ID
				return page, nil
			}
		}
		if len(rows) < f.Limit {
			return page, nil
		}
		f.Cursor = rows[len(rows)-1].ID
	}
}
