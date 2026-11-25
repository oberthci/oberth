package store

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// totalsPair reads one page in one snapshot and returns its totals from the
// cached facts (cached false: declined) and from the scan.
func totalsPair(t *testing.T, s *Store, set issueScope, f model.IssueFilter) (cached, scan stateport.IssuePage, used bool, scanErr error) {
	t.Helper()
	tx, err := s.readers.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	requireOK(t, err)
	defer func() { _ = tx.Rollback() }()
	list := issueList{tx: tx, filter: f}
	list.set, list.args = issueSet(set.sql, set.args, f, s.options.Clock())
	facts, cut, ok := s.snapshotFacts(t.Context(), tx, list.args[:2])
	rows, err := list.rows(t.Context())
	if err != nil {
		return cached, scan, false, err
	}
	cached.Issues, scan.Issues = rows, rows
	used = ok && list.cachedTotals(&cached, facts, cut, set.in)
	return cached, scan, used, list.totals(t.Context(), &scan)
}

func requireTotalsMatchScan(t *testing.T, s *Store, set issueScope, f model.IssueFilter) bool {
	t.Helper()
	cached, scan, used, err := totalsPair(t, s, set, f)
	if used != (err == nil) {
		t.Fatalf("cache used %v, scan error %v, filter %+v", used, err, f)
	}
	if used && !reflect.DeepEqual(cached, scan) {
		t.Fatalf("cached totals differ from the scan\nfilter %+v\ncached %+v %d %d\nscan   %+v %d %d", f, cached.Counts, cached.Total, cached.Offset, scan.Counts, scan.Total, scan.Offset)
	}
	return used
}

func importIssue(t *testing.T, s *Store, records ...model.IssueImportRecord) {
	t.Helper()
	_, err := s.ImportIssues(t.Context(), importAdmin(), stateport.IssueImport{Records: records, FileSHA256: strings.Repeat(fmt.Sprintf("%x", records[0].ID%16), 64)})
	requireOK(t, err)
}

// spec describes one imported manual issue; age is the time since its update.
type spec struct {
	upstream, repo string
	status         model.IssueWorkStatus
	closed         bool
	age            time.Duration
}

func (v spec) record(id int64) model.IssueImportRecord {
	at := testOptions().Clock().Add(-v.age)
	r := model.IssueImportRecord{ID: id, Upstream: v.upstream, Repo: v.repo, Title: "task", Body: "body", State: model.IssueOpen,
		CreatedAt: at, UpdatedAt: at, Author: "source", Kind: model.IssueManual, WorkStatus: v.status, Occurrences: 1}
	if v.closed {
		r.State, r.ClosedAt = model.IssueClosed, &at
	}
	return r
}

func totalsStatus(i int) model.IssueWorkStatus {
	return [...]model.IssueWorkStatus{model.IssueTodo, model.IssueActive, model.IssueWaiting, model.IssueReview}[i%4]
}

func totalsWindow(i int) time.Duration {
	return [...]time.Duration{0, time.Hour, 24 * time.Hour, 36 * time.Hour}[i%4]
}

func totalsScopes() []issueScope {
	return []issueScope{
		repositoryIssueScope(nil), repositoryIssueScope([]string{}), repositoryIssueScope([]string{"origin/team/repo"}),
		repositoryIssueScope([]string{"origin/team/tool", "other/team/repo"}),
		upstreamIssueScope(stateport.IssueScope{}), upstreamIssueScope(stateport.IssueScope{Global: true}),
		upstreamIssueScope(stateport.IssueScope{Upstreams: []string{}}), upstreamIssueScope(stateport.IssueScope{Upstreams: []string{"origin"}}),
		upstreamIssueScope(stateport.IssueScope{Upstreams: []string{"origin", "other"}, Global: true}),
	}
}

// TestIssueTotalsTable pins the scan's edge rules: scope forms, the stale
// cutoff boundary, non-string JSON values and records outside the set.
func TestIssueTotalsTable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	importIssue(t, s,
		spec{"origin", "origin/team/repo", model.IssueActive, false, 2 * time.Hour}.record(1),
		spec{"origin", "origin/team/repo", model.IssueTodo, false, 24 * time.Hour}.record(2),
		spec{"origin", "", model.IssueReview, true, 48 * time.Hour}.record(3),
		spec{"global", "", model.IssueWaiting, false, 30 * time.Hour}.record(4),
		spec{"", "", model.IssueActive, false, time.Hour}.record(5),
		spec{"other", "other/team/repo", model.IssueTodo, false, 72 * time.Hour}.record(6))
	raw := pool(s.path, "rw", testOptions(), 1)
	defer func() { requireOK(t, raw.Close()) }()
	for _, value := range []string{`{"work_status":{"a":1},"kind":"manual"}`, `{"work_status":"active","updated_at":12}`, `[1]`, `"text"`} {
		_, err := raw.ExecContext(t.Context(), `INSERT INTO issues(revision,upstream,repo,state,value) VALUES(1,'origin','origin/team/repo','open',?)`, value)
		requireOK(t, err)
	}
	windows := model.IssueWindows{General: 24 * time.Hour, Active: 2 * time.Hour}
	for _, set := range totalsScopes() {
		for _, f := range []model.IssueFilter{
			{Limit: 2, Windows: windows, Before: 7},
			{Limit: 2, Windows: windows, Stale: true, State: "open", Before: 7},
			{Limit: 1, Windows: windows, WorkStatus: model.IssueActive, Before: 5},
			{Limit: 1, Windows: model.IssueWindows{General: 24 * time.Hour}, After: 1},
			{Limit: 3, Kind: model.IssueManual, Repo: "origin/team/repo", State: "all", Before: 7},
		} {
			if !requireTotalsMatchScan(t, s, set, f) {
				t.Fatal("well-formed records declined the cache", f)
			}
		}
	}
	page, err := s.ScopedIssues(t.Context(), stateport.IssueScope{Upstreams: []string{"origin"}}, model.IssueFilter{Limit: 2, Windows: windows, Before: 7})
	requireOK(t, err)
	if want := (model.IssueCounts{Open: 6, Closed: 1, Todo: 1, Active: 2, Stale: 3}); page.Counts != want {
		t.Fatal("stale boundary or JSON types counted wrong", page.Counts)
	}
}

// TestMalformedIssueOnlyFailsItsOwnScope keeps the scan's answer for a scope
// that holds a malformed record and the cache for every other scope.
func TestMalformedIssueOnlyFailsItsOwnScope(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	importIssue(t, s, spec{"origin", "origin/team/repo", model.IssueTodo, false, time.Hour}.record(1))
	raw := pool(s.path, "rw", testOptions(), 1)
	defer func() { requireOK(t, raw.Close()) }()
	_, err := raw.ExecContext(t.Context(), `INSERT INTO issues(revision,upstream,repo,state,value) VALUES(1,'other','other/team/repo','open','not-json')`)
	requireOK(t, err)
	f := model.IssueFilter{Limit: 5}
	if !requireTotalsMatchScan(t, s, upstreamIssueScope(stateport.IssueScope{Upstreams: []string{"origin"}}), f) {
		t.Fatal("an unrelated malformed record declined the cache")
	}
	if requireTotalsMatchScan(t, s, upstreamIssueScope(stateport.IssueScope{Upstreams: []string{"other"}}), f) {
		t.Fatal("a malformed record in scope was counted")
	}
	if _, err := s.ScopedIssues(t.Context(), stateport.IssueScope{Upstreams: []string{"origin"}}, f); err != nil {
		t.Fatal(err)
	}
}

// TestIssueWritesUpdateCachedFacts proves that a write of this process
// replaces its facts at commit: the next read uses the cache and reads no
// other record. The planted record keeps its ID and revision, so only a
// rebuild would see it.
func TestIssueWritesUpdateCachedFacts(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	todo := spec{"origin", "origin/team/repo", model.IssueTodo, false, time.Hour}
	importIssue(t, s, todo.record(1), todo.record(2))
	scope := stateport.IssueScope{Upstreams: []string{"origin"}}
	f := model.IssueFilter{Limit: 1}
	_, err := s.ScopedIssues(t.Context(), scope, f)
	requireOK(t, err)
	raw := pool(s.path, "rw", testOptions(), 1)
	_, err = raw.ExecContext(t.Context(), `UPDATE issues SET value='not-json' WHERE id=1`)
	requireOK(t, err)
	requireOK(t, raw.Close())
	status := model.IssueActive
	_, err = s.UpdateIssue(t.Context(), importAdmin(), model.IssuePatch{ID: 2, WorkStatus: &status})
	requireOK(t, err)
	page, err := s.ScopedIssues(t.Context(), scope, f)
	requireOK(t, err)
	if page.Counts.Active != 1 || page.Counts.Todo != 1 || len(page.Issues) != 1 || page.Issues[0].ID != 2 {
		t.Fatal("write did not update the cached facts", page.Counts)
	}
}

// TestOtherProcessReplacementRebuilds: another process deletes an issue and
// imports it again under the same ID and revision; its audit rows move the
// head, so the next read rebuilds instead of counting the old facts.
func TestOtherProcessReplacementRebuilds(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	importIssue(t, s, spec{"origin", "origin/team/repo", model.IssueTodo, false, time.Hour}.record(1))
	set := upstreamIssueScope(stateport.IssueScope{})
	requireTotalsMatchScan(t, s, set, model.IssueFilter{Limit: 1})
	other, err := Open(t.Context(), s.path, testOptions())
	requireOK(t, err)
	defer func() { requireOK(t, other.Close()) }()
	reimport(t, other, 1, model.IssueWaiting)
	cached, _, used, err := totalsPair(t, s, set, model.IssueFilter{Limit: 1})
	requireOK(t, err)
	if !used || cached.Counts.Waiting != 1 || cached.Counts.Todo != 0 {
		t.Fatal("replacement by another process was not seen", cached.Counts)
	}
}

// TestFailedFactUpdateDropsCache: a write whose facts cannot be read keeps its
// commit and leaves the next read to rebuild from the snapshot.
func TestFailedFactUpdateDropsCache(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	importIssue(t, s, spec{"origin", "origin/team/repo", model.IssueTodo, false, time.Hour}.record(1))
	set := upstreamIssueScope(stateport.IssueScope{})
	requireTotalsMatchScan(t, s, set, model.IssueFilter{Limit: 1})
	err := s.change(t.Context(), mutation{"administrator", "issue.update", "issue", "1", nil}, func(tx *sql.Tx) error {
		issue, err := readIssue(t.Context(), tx, 1)
		if err != nil {
			return err
		}
		issue.WorkStatus, issue.Revision = model.IssueReview, issue.Revision+1
		for id := range int64(40000) { // more parameters than one fact read accepts
			s.noteIssueWrite(id + 2)
		}
		return s.saveIssue(t.Context(), tx, &issue, false)
	})
	requireOK(t, err)
	s.issueFacts.mu.Lock()
	dropped := s.issueFacts.facts == nil
	s.issueFacts.mu.Unlock()
	if !dropped {
		t.Fatal("failed fact read kept the cache")
	}
	cached, _, used, err := totalsPair(t, s, set, model.IssueFilter{Limit: 1})
	requireOK(t, err)
	if !used || cached.Counts.Review != 1 {
		t.Fatal("rebuild missed the committed write", cached.Counts)
	}
}

// TestIssueCommitHoldsFactsLock: an issue write commits inside the facts
// critical section, so no reader pins a snapshot between commit and update.
func TestIssueCommitHoldsFactsLock(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	importIssue(t, s, spec{"origin", "origin/team/repo", model.IssueTodo, false, time.Hour}.record(1))
	s.issueFacts.mu.Lock()
	done := make(chan error, 1)
	go func() {
		status := model.IssueActive
		_, err := s.UpdateIssue(t.Context(), importAdmin(), model.IssuePatch{ID: 1, WorkStatus: &status})
		done <- err
	}()
	var early int64
	select {
	case err := <-done:
		s.issueFacts.mu.Unlock()
		t.Fatal("issue write finished while the facts lock was held", err)
	case <-time.After(100 * time.Millisecond):
		requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT revision FROM issues WHERE id=1`).Scan(&early))
	}
	s.issueFacts.mu.Unlock()
	requireOK(t, <-done)
	if early != 1 {
		t.Fatal("issue write committed outside the facts lock", early)
	}
}

// TestIssueTotalsConcurrentWriters compares cache and scan in one snapshot
// while writers of this process and of another process change issues,
// including deleting and importing the same ID at the same revision.
func TestIssueTotalsConcurrentWriters(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	other, err := Open(t.Context(), s.path, testOptions())
	requireOK(t, err)
	defer func() { requireOK(t, other.Close()) }()
	for id := int64(1); id <= 40; id++ {
		importIssue(t, s, spec{"origin", "origin/team/repo", totalsStatus(int(id)), id%5 == 0, time.Duration(id) * time.Hour}.record(id))
	}
	var writers, readers sync.WaitGroup
	stop := make(chan struct{})
	for w := range 4 {
		writer := s
		if w == 3 {
			writer = other
		}
		writers.Go(func() { churnIssues(t, writer, w, stop) })
	}
	for r := range 4 {
		readers.Go(func() {
			sets := totalsScopes()
			for i := range 60 {
				f := model.IssueFilter{Limit: 3, Before: int64(1 + i%40), Windows: model.IssueWindows{General: 24 * time.Hour, Active: time.Hour}}
				cached, scan, used, err := totalsPair(t, s, sets[(r+i)%len(sets)], f)
				if err != nil || !used || !reflect.DeepEqual(cached, scan) {
					t.Errorf("cached %+v scan %+v used %v err %v", cached.Counts, scan.Counts, used, err)
					return
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	writers.Wait()
}

// churnIssues changes work statuses until stop; writer 0 also deletes issues
// and imports them again under the same ID and revision.
func churnIssues(t *testing.T, s *Store, w int, stop <-chan struct{}) {
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		default:
		}
		id := int64(1 + (w*10+i)%40)
		status := totalsStatus(i + w)
		if w == 0 && i%2 == 0 {
			reimport(t, s, id, status)
			continue
		}
		_, _ = s.UpdateIssue(t.Context(), importAdmin(), model.IssuePatch{ID: id, WorkStatus: &status})
	}
}

func reimport(t *testing.T, s *Store, id int64, status model.IssueWorkStatus) {
	issue, err := s.Issue(t.Context(), id)
	if err != nil || issue.Revision != 1 || s.DeleteIssue(t.Context(), importAdmin(), stateport.IssueClose{ID: id, Revision: 1}) != nil {
		return
	}
	v := spec{issue.Upstream, issue.Repo, status, issue.State == model.IssueClosed, time.Duration(id) * time.Hour}
	_, err = s.ImportIssues(t.Context(), importAdmin(), stateport.IssueImport{Records: []model.IssueImportRecord{v.record(id)}, FileSHA256: strings.Repeat(fmt.Sprintf("%x", id%16), 64)})
	if err != nil {
		t.Error(err)
	}
}

// TestIssueTotalsAddNoSchemaObjects: reads and writes with the cache leave the
// schema-6 layout exactly as it was, so main's executable still opens it.
func TestIssueTotalsAddNoSchemaObjects(t *testing.T) {
	t.Parallel()
	path := schema6Fixture(t)
	layout := func() string {
		db := pool(path, "ro", testOptions(), 1)
		defer func() { requireOK(t, db.Close()) }()
		var out string
		requireOK(t, db.QueryRowContext(t.Context(), `SELECT group_concat(type||' '||name||' '||coalesce(sql,''),char(10)) FROM (SELECT * FROM sqlite_master ORDER BY type,name)`).Scan(&out))
		return out
	}
	before := layout()
	s, err := Open(t.Context(), path, testOptions())
	requireOK(t, err)
	scope := stateport.IssueScope{Upstreams: []string{"origin"}, Global: true}
	_, err = s.ScopedIssues(t.Context(), scope, model.IssueFilter{Limit: 5})
	requireOK(t, err)
	status := model.IssueActive
	_, err = s.UpdateIssue(t.Context(), importAdmin(), model.IssuePatch{ID: 50, WorkStatus: &status})
	requireOK(t, err)
	page, err := s.ScopedIssues(t.Context(), scope, model.IssueFilter{Limit: 5})
	requireOK(t, err)
	if page.Counts.Active == 0 {
		t.Fatal("schema-six issue write not counted", page.Counts)
	}
	requireOK(t, s.Close())
	if after := layout(); after != before {
		t.Fatal("issue totals changed the persistent schema\n", before, "\n", after)
	}
	probeMainSchema6(t, path)
	s, err = Open(t.Context(), path, testOptions())
	requireOK(t, err)
	assertSchema6Records(t, s)
	requireOK(t, checkSchema(t.Context(), s.readers))
	requireOK(t, s.Close())
}
