package fake

import (
	"context"

	"github.com/oberthci/oberth/internal/stepmodel"
)

func (f *Runs) RunCounts(ctx context.Context, scope []string) (map[stepmodel.RunStatus]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "RunCounts", false); err != nil {
		return nil, err
	}
	counts := map[stepmodel.RunStatus]int{}
	for _, r := range f.data.runs {
		if visibleRun(scope, r.Repo) {
			counts[r.Status]++
		}
	}
	return counts, nil
}
