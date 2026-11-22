package fake

import (
	"cmp"
	"context"
	"slices"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// RecentRuns keeps each named repository's newest RecentRunLimit runs, oldest first.
func (f *Runs) RecentRuns(ctx context.Context, repositories []string) (map[string][]stateport.RunMark, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "RecentRuns", false); err != nil {
		return nil, err
	}
	if repositories == nil || len(repositories) > stateport.RunPageLimit {
		return nil, runProblem(publicerr.InvalidInput)
	}
	rows := make([]stateport.Run, 0)
	for _, r := range f.data.runs {
		if slices.Contains(repositories, r.Repo) {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, func(a, b stateport.Run) int { return cmp.Compare(a.Sequence, b.Sequence) })
	out := map[string][]stateport.RunMark{}
	for _, r := range rows {
		marks := append(out[r.Repo], stateport.RunMark{ID: r.ID, Status: r.Status, Ref: r.Ref, CreatedAt: r.CreatedAt})
		out[r.Repo] = marks[max(0, len(marks)-stateport.RecentRunLimit):]
	}
	return out, nil
}
