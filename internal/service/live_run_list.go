package service

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"time"
)

// heldRunPage filters live candidates before the requested page limit.
func (s *Service) heldRunPage(ctx context.Context, f stateport.RunFilter) (apicontract.RunPage, error) {
	page := apicontract.RunPage{Runs: []stepmodel.RunSummary{}}
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
			page.Runs = append(page.Runs, summary(r.RunSummary))
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
