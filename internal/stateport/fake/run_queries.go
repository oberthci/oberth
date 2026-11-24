package fake

import (
	"context"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func (f *Runs) Run(ctx context.Context, id string) (stateport.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "Run", false); err != nil {
		return stateport.Run{}, err
	}
	r, ok := f.data.runs[id]
	if !ok {
		return r, runProblem(publicerr.NotFound)
	}

	return r.Clone(), nil
}

func (f *Runs) RunState(ctx context.Context, id string) (stateport.RunState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "RunState", false); err != nil {
		return stateport.RunState{}, err
	}
	r, ok := f.data.runs[id]
	if !ok {
		return stateport.RunState{}, runProblem(publicerr.NotFound)
	}
	return stateport.RunState{RunSummary: r.RunSummary.Clone(), Revision: r.Revision}, nil
}

func (f *Runs) ResolveRun(ctx context.Context, scope []string, prefix string) (stateport.Run, error) {
	return f.resolveRun(ctx, scope, prefix, false)
}

func (f *Runs) resolveRun(ctx context.Context, scope []string, prefix string, record bool) (stateport.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "ResolveRun", false); err != nil {
		return stateport.Run{}, err
	}
	if prefix == "" {
		return stateport.Run{}, runProblem(publicerr.InvalidInput)
	}
	var out stateport.Run
	count := 0
	for id, r := range f.data.runs {
		if visibleRun(scope, r.Repo) && strings.HasPrefix(id, prefix) {
			if id == prefix {

				return queryProjection(r, record), nil
			}
			out = r
			count++
		}
	}
	if count == 0 {
		return out, runProblem(publicerr.NotFound)
	}
	if count > 1 {
		return out, runProblem(publicerr.Ambiguous)
	}

	return queryProjection(out, record), nil
}

func (f *Runs) LookupRun(ctx context.Context, scope []string, prefix string) (stateport.Run, error) {
	return f.resolveRun(ctx, scope, prefix, true)
}

func queryProjection(r stateport.Run, record bool) stateport.Run {
	if record {
		r.Admission = stateport.Run{}.Admission
	}
	return r.Clone()
}

func (f *Runs) RunByRef(ctx context.Context, scope []string, ref string) (stateport.Run, error) {
	rows, err := f.Runs(ctx, stateport.RunFilter{Repositories: scope, Ref: ref, Limit: 1})
	if err != nil {
		return stateport.Run{}, err
	}
	if len(rows) == 0 {
		return stateport.Run{}, runProblem(publicerr.NotFound)
	}
	return rows[0], nil
}

func (f *Runs) RunByCommit(ctx context.Context, scope []string, prefix string) (stateport.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "RunByCommit", false); err != nil {
		return stateport.Run{}, err
	}
	if !stateport.ValidCommitPrefix(prefix) {
		return stateport.Run{}, runProblem(publicerr.InvalidInput)
	}
	prefix = strings.ToLower(prefix)
	var newest stateport.Run
	commits := map[string]bool{}
	for _, r := range f.data.runs {
		if !visibleRun(scope, r.Repo) {
			continue
		}
		if strings.HasPrefix(r.ObjectSHA, prefix) || r.CommitSHA != nil && strings.HasPrefix(*r.CommitSHA, prefix) {
			key := r.ObjectSHA
			if r.CommitSHA != nil {
				key = *r.CommitSHA
			}
			commits[key] = true
			if r.Sequence > newest.Sequence {
				newest = r
			}
		}
	}
	if len(commits) > 1 {
		return newest, runProblem(publicerr.Ambiguous)
	}
	if len(commits) == 0 {
		return newest, runProblem(publicerr.NotFound)
	}

	return newest.Clone(), nil
}

func (f *Runs) Runs(ctx context.Context, q stateport.RunFilter) ([]stateport.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "Runs", false); err != nil {
		return nil, err
	}
	if err := fakeRunFilter(q); err != nil {
		return nil, err
	}
	cursor, err := f.data.cursor(q)
	if err != nil {
		return nil, err
	}
	out := f.data.selectedRuns(q)
	slices.SortFunc(out, func(a, b stateport.Run) int {
		if a.Sequence > b.Sequence {
			return -1
		}
		if a.Sequence < b.Sequence {
			return 1
		}
		return 0
	})
	out = pageRuns(out, q, cursor)
	for i := range out {
		out[i].Admission = stateport.Run{}.Admission
		out[i].Findings = nil
		out[i] = out[i].Clone()
	}
	return out, nil
}

// The caller holds the store lock. Keep raw values until the page is selected;
// discarded histories must not clone admissions or findings.
func (m *runMemory) selectedRuns(q stateport.RunFilter) []stateport.Run {
	out := make([]stateport.Run, 0)
	latest := make(map[string]stateport.Run)
	for _, r := range m.runs {
		if !selectedRun(r, q) {
			continue
		}
		if !q.Latest && !q.LatestRepository {
			out = append(out, r)
			continue
		}
		key := r.Repo
		if !q.LatestRepository {
			key += "\x00" + r.Ref
		}
		if r.Sequence > latest[key].Sequence {
			latest[key] = r
		}
	}
	for _, r := range latest {
		out = append(out, r)
	}
	return out
}

func selectedRun(r stateport.Run, q stateport.RunFilter) bool {
	if !visibleRun(q.Repositories, r.Repo) || q.Status != "" && r.Status != q.Status || q.Ref != "" && r.Ref != q.Ref {
		return false
	}
	if !runMetadata(r, q) {
		return false
	}
	if q.CommitSHA != "" && (r.CommitSHA == nil || *r.CommitSHA != q.CommitSHA) {
		return false
	}
	return q.RefGlob == "" || stateport.MatchRefGlob(q.RefGlob, r.Ref)
}

func pageRuns(rows []stateport.Run, q stateport.RunFilter, cursor int64) []stateport.Run {
	out := make([]stateport.Run, 0)
	seen := map[string]bool{}
	for _, r := range rows {
		key := r.Repo + "\x00" + r.Ref
		if q.LatestRepository {
			key = r.Repo
		}
		if (q.Latest || q.LatestRepository) && seen[key] {
			continue
		}
		seen[key] = true
		if cursor > 0 && r.Sequence >= cursor {
			continue
		}
		out = append(out, r)
		if len(out) == q.Limit {
			break
		}
	}
	return out
}

func fakeRunFilter(q stateport.RunFilter) error {
	if q.Limit < 1 || q.Limit > stateport.RunPageLimit || q.Before < 0 || q.Status != "" && !q.Status.Valid() {
		return runProblem(publicerr.InvalidInput)
	}
	if q.Trigger != "" && !q.Trigger.Valid() {
		return publicerr.New(publicerr.InvalidInput, "run trigger is invalid", "Select a valid run trigger.", nil)
	}
	if strings.IndexByte(q.Cursor, 0) >= 0 {
		return runProblem(publicerr.InvalidInput)
	}
	if q.RefGlob != "" && !stateport.ValidRefGlob(q.RefGlob) {
		return runProblem(publicerr.InvalidInput)
	}
	return nil
}

func runMetadata(r stateport.Run, q stateport.RunFilter) bool {
	if q.Actor != "" && r.Origin.Actor != q.Actor || q.Trigger != "" && r.Origin.Trigger != q.Trigger {
		return false
	}
	return true
}

func (m *runMemory) cursor(q stateport.RunFilter) (int64, error) {
	cursor := q.Before
	if q.Cursor == "" {
		return cursor, nil
	}
	r, ok := m.runs[q.Cursor]
	if !ok || !visibleRun(q.Repositories, r.Repo) {
		return 0, runProblem(publicerr.NotFound)
	}
	if cursor == 0 || r.Sequence < cursor {
		cursor = r.Sequence
	}
	return cursor, nil
}
