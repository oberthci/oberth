package store

import (
	"database/sql"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func randomTotalsFilter(r *rand.Rand, last int64) model.IssueFilter {
	f := model.IssueFilter{Limit: 1 + r.IntN(5), State: []string{"", "all", "open", "closed"}[r.IntN(4)], Stale: r.IntN(3) == 0,
		Windows: model.IssueWindows{General: totalsWindow(r.IntN(4)), Active: totalsWindow(r.IntN(4))}}
	if r.IntN(3) == 0 {
		f.WorkStatus = totalsStatus(r.IntN(4))
	}
	if r.IntN(4) == 0 {
		f.Kind = []model.IssueKind{model.IssueManual, model.IssueCI}[r.IntN(2)]
	}
	if r.IntN(4) == 0 {
		f.Repo = []string{"origin/team/repo", "other/team/repo", "missing/x/y"}[r.IntN(3)]
	}
	switch r.IntN(3) {
	case 0:
		f.Before = 1 + r.Int64N(last+2)
	case 1:
		f.After = r.Int64N(last + 2)
	}
	return f
}

// randomIssues applies random writes through this process, a second process
// and raw SQL; live holds the IDs that the issue API can still decode.
type randomIssues struct {
	t         *testing.T
	r         *rand.Rand
	raw       *sql.DB
	live      map[int64]bool
	next      int64
	s, writer *Store
}

func (w *randomIssues) pick() int64 {
	if len(w.live) == 0 {
		return 0
	}
	ids := slices.Sorted(maps.Keys(w.live))
	return ids[w.r.IntN(len(ids))]
}

func (w *randomIssues) step(other *Store) {
	w.writer = w.s
	if w.r.IntN(4) == 0 {
		w.writer = other
	}
	id := w.pick()
	switch op := w.r.IntN(9); {
	case op < 3 || id == 0:
		w.create()
	case op == 3:
		status := totalsStatus(w.r.IntN(4))
		_, err := w.writer.UpdateIssue(w.t.Context(), importAdmin(), model.IssuePatch{ID: id, WorkStatus: &status})
		requireOK(w.t, err)
	case op == 4:
		w.toggle(id)
	case op == 5:
		w.replace(id)
	case op == 6:
		requireOK(w.t, w.writer.ChangeIssueLock(w.t.Context(), importAdmin(), stateport.IssueLockRequest{ID: id, Owner: "administrator"}))
	case op == 7:
		_, err := w.raw.ExecContext(w.t.Context(), `UPDATE issues SET revision=revision+1, value=json_set(value,'$.work_status',?,'$.kind',?) WHERE id=?`,
			[]any{"active", 7, nil, "todo"}[w.r.IntN(4)], []any{"ci", "manual", 3}[w.r.IntN(3)], id)
		requireOK(w.t, err)
		delete(w.live, id) // its record may no longer decode, so only lists see it
	default:
		_, err := w.raw.ExecContext(w.t.Context(), `INSERT INTO issues(id,revision,upstream,repo,state,value) VALUES(?,1,'other','other/team/repo','open','not-json')`, w.next)
		requireOK(w.t, err)
		w.next++
	}
}

func (w *randomIssues) create() {
	places := [][2]string{{"origin", "origin/team/repo"}, {"origin", "origin/team/tool"}, {"origin", ""}, {"other", "other/team/repo"}, {"other", ""}, {"global", ""}, {"", ""}}
	place := places[w.r.IntN(len(places))]
	v := spec{place[0], place[1], totalsStatus(w.r.IntN(4)), w.r.IntN(3) == 0, time.Duration(w.r.IntN(60)) * time.Hour}
	if w.r.IntN(4) == 0 {
		v.age = 24 * time.Hour // exactly at a window's cutoff
	}
	importIssue(w.t, w.writer, v.record(w.next))
	w.live[w.next] = true
	w.next++
}

func (w *randomIssues) toggle(id int64) {
	issue, err := w.writer.Issue(w.t.Context(), id)
	requireOK(w.t, err)
	request := stateport.IssueClose{ID: id, Revision: issue.Revision, WorkID: issue.CIWorkID}
	if issue.State == model.IssueOpen {
		requireOK(w.t, w.writer.CloseIssue(w.t.Context(), importAdmin(), request))
		return
	}
	requireOK(w.t, w.writer.ReopenIssue(w.t.Context(), importAdmin(), request))
}

// replace deletes an issue and, half of the time, imports the same ID at the
// same revision with other facts.
func (w *randomIssues) replace(id int64) {
	issue, err := w.writer.Issue(w.t.Context(), id)
	requireOK(w.t, err)
	requireOK(w.t, w.writer.DeleteIssue(w.t.Context(), importAdmin(), stateport.IssueClose{ID: id, Revision: issue.Revision}))
	delete(w.live, id)
	if issue.Revision == 1 && w.r.IntN(2) == 0 {
		v := spec{issue.Upstream, issue.Repo, totalsStatus(w.r.IntN(4)), issue.State == model.IssueOpen, time.Duration(w.r.IntN(60)) * time.Hour}
		importIssue(w.t, w.writer, v.record(id))
		w.live[id] = true
	}
}

// TestIssueTotalsMatchScanUnderRandomWrites applies random writes of this
// process and of a second process, and compares every read with the scan.
func TestIssueTotalsMatchScanUnderRandomWrites(t *testing.T) {
	t.Parallel()
	for seed := range uint64(4) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			s := testStore(t)
			other, err := Open(t.Context(), s.path, testOptions())
			requireOK(t, err)
			t.Cleanup(func() { requireOK(t, other.Close()) })
			w := &randomIssues{t: t, r: rand.New(rand.NewPCG(seed, 1834)), raw: pool(s.path, "rw", testOptions(), 1), live: map[int64]bool{}, next: 1, s: s}
			t.Cleanup(func() { requireOK(t, w.raw.Close()) })
			used, scopes := 0, totalsScopes()
			for range 160 {
				w.step(other)
				for range 4 {
					if requireTotalsMatchScan(t, s, scopes[w.r.IntN(len(scopes))], randomTotalsFilter(w.r, w.next)) {
						used++
					}
				}
			}
			if used < 200 {
				t.Fatal("cached totals were rarely used", used)
			}
		})
	}
}
