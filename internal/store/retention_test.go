package store

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestIssueFiltersAndStalenessAreReadOnly(t *testing.T) {
	t.Parallel()
	options := testOptions()
	at := options.Clock()
	var elapsed atomic.Int64
	options.Clock = func() time.Time { return at.Add(time.Duration(elapsed.Load())) }
	s, err := Create(t.Context(), filepath.Join(t.TempDir(), "state.sqlite"), options)
	requireOK(t, err)
	defer func() { requireOK(t, s.Close()) }()
	first, err := s.CreateIssue(t.Context(), stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "needle", Body: "detail", Repo: "origin/team/repo", Upstream: "origin"})
	requireOK(t, err)
	_, err = s.CreateIssue(t.Context(), stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "another", Repo: "origin/team/repo", Upstream: "origin"})
	requireOK(t, err)
	page, err := s.Issues(t.Context(), []string{"origin/team/repo"}, model.IssueFilter{Query: "needle", Kind: model.IssueManual, WorkStatus: model.IssueTodo, Limit: 1})
	requireOK(t, err)
	if len(page) != 1 || page[0].ID != first.ID {
		t.Fatal("filter ran after pagination")
	}
	elapsed.Store(int64(time.Hour))
	before, err := s.Issue(t.Context(), first.ID)
	requireOK(t, err)
	for range 2 {
		rows, err := s.Issues(t.Context(), []string{"origin/team/repo"}, model.IssueFilter{StaleAfter: "1h", ActiveStaleAfter: "1m", Limit: 200})
		requireOK(t, err)
		if len(rows) != 2 || !rows[0].Stale || !rows[1].Stale {
			t.Fatal("staleness boundary was not observed")
		}
	}
	after, err := s.Issue(t.Context(), first.ID)
	requireOK(t, err)
	if encode(before) != encode(after) {
		t.Fatal("read mutated lock, revision or timestamps")
	}
}

func TestProgressRetentionKeepsNewest10000WithoutEvictingTerminal(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	event := model.AgentEvent{Repo: "origin/team/repo", Kind: string(model.EventRunFinished), At: s.options.Clock(), Summary: "run finished"}
	terminalID, err := eventForTest(t.Context(), s, stateport.EventWrite{Event: event, Dedupe: "terminal"})
	requireOK(t, err)
	event.Kind = string(model.EventRunUpdated)
	event.Summary = "run observed"
	tx, err := s.writer.BeginTx(t.Context(), nil)
	requireOK(t, err)
	defer func() { _ = tx.Rollback() }()
	for index := 0; index < 10001; index++ {
		_, err = tx.ExecContext(t.Context(), `INSERT INTO events(repo,kind,dedupe,at,value) VALUES(?,?,?,?,?)`, event.Repo, event.Kind, fmt.Sprintf("progress-%d", index), event.At.Format(timestampFormat), encode(event))
		requireOK(t, err)
	}
	requireOK(t, tx.Commit())
	requireOK(t, s.RetainEvents(t.Context(), event.At.Add(-time.Hour), event.At.Add(-time.Hour)))
	var progressCount, terminalCount int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM events WHERE kind='run.updated'`).Scan(&progressCount))
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM events WHERE id=?`, terminalID).Scan(&terminalCount))
	if progressCount != 10000 || terminalCount != 1 {
		t.Fatal("progress retention consumed wrong partition", progressCount, terminalCount)
	}
}
