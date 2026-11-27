package store

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestScopedChangeCoverage(t *testing.T) {
	t.Parallel()
	t.Run("write inventory", TestScopedChangeWriteInventory)
	s := testStore(t)
	r, _ := accepted(t, s, "scoped")
	subs := make([]stateport.Subscription, 100)
	for i := range subs {
		id := fmt.Sprint("unrelated-", i)
		if i < 10 {
			id = r.ID
		}
		subs[i] = s.Subscribe(stateport.ChangeScope{Runs: []string{id}, Invalidations: []string{r.Repo}})
		defer subs[i].Close()
	}
	p := stateport.Progress{Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepQueued, DisplayReason: "waiting for a node"}}}
	requireOK(t, s.SaveProgress(t.Context(), r.ID, p))
	for i, sub := range subs {
		assertHint(t, sub, i < 10)
	}
	before, err := s.Events(t.Context(), stateport.EventFilter{Limit: 500})
	requireOK(t, err)
	p.Steps[0].DisplayReason = "starting containers"
	requireOK(t, s.SaveProgress(t.Context(), r.ID, p))
	p.Steps[0].DisplayReason = "image pull failed"
	requireOK(t, s.SaveProgress(t.Context(), r.ID, p))
	for i, sub := range subs {
		assertHint(t, sub, i < 10)
		assertHint(t, sub, false)
	}
	page, err := s.Events(t.Context(), stateport.EventFilter{RunIDs: []string{r.ID}, After: before.LatestID, Limit: 500})
	requireOK(t, err)
	if len(page.Events) != 2 || page.Events[0].ID >= page.Events[1].ID {
		t.Fatal("coalescing discarded durable events", page)
	}
	requireOK(t, s.SaveProgress(t.Context(), r.ID, p))
	for _, sub := range subs {
		assertHint(t, sub, false)
	}
	state, err := s.RunState(t.Context(), r.ID)
	requireOK(t, err)
	if state.Revision != r.Revision {
		t.Fatal("observation changed control revision")
	}
	t.Run("silent rollback and mixed facts", func(t *testing.T) { scopedTransactionFacts(t, s, r) })
	t.Run("pruning and cleanup", func(t *testing.T) { scopedPruning(t, s, r) })
}

func assertHint(t *testing.T, sub stateport.Subscription, want bool) {
	t.Helper()
	select {
	case <-sub.C:
		if !want {
			t.Fatal("unrelated or duplicate wake")
		}
	default:
		if want {
			t.Fatal("committed fact lost its scoped wake")
		}
	}
}

func scopedTransactionFacts(t *testing.T, s *Store, r stateport.Run) {
	watch := s.Subscribe(stateport.ChangeScope{Runs: []string{r.ID}, Invalidations: []string{r.Repo}})
	defer watch.Close()
	issue := s.Subscribe(stateport.ChangeScope{Upstreams: []string{"other"}})
	defer issue.Close()
	requireOK(t, s.observe(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE native_steps SET value=value WHERE run_id=?`, r.ID)
		return err
	}))
	assertHint(t, watch, false)
	rollback := errors.New("rollback control")
	err := s.observe(t.Context(), func(*sql.Tx) error {
		s.noteChange(stateport.Change{Repository: r.Repo, Run: r.ID})
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	assertHint(t, watch, false)
	requireOK(t, s.observe(t.Context(), func(tx *sql.Tx) error {
		if err := s.saveVerdict(t.Context(), tx, &r, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed}); err != nil {
			return err
		}
		v := model.Issue{Kind: model.IssueManual, Title: "scope", Upstream: "other", State: model.IssueOpen, WorkStatus: model.IssueTodo}
		if err := s.saveIssue(t.Context(), tx, &v, true); err != nil {
			return err
		}
		return s.appendAudit(t.Context(), tx, mutation{"observer", "identity.changed", "key", "identity", nil})
	}))
	assertHint(t, watch, true)
	assertHint(t, issue, true)
}

func scopedPruning(t *testing.T, s *Store, r stateport.Run) {
	watch := s.Subscribe(stateport.ChangeScope{Runs: []string{r.ID}, Invalidations: []string{r.Repo}})
	defer watch.Close()
	repo := s.Subscribe(stateport.ChangeScope{Repositories: []string{r.Repo}})
	defer repo.Close()
	scope := stateport.Change{Repository: r.Repo}
	requireOK(t, s.observe(t.Context(), func(*sql.Tx) error { s.noteChange(scope); return nil }))
	assertHint(t, repo, true)
	assertHint(t, watch, false)
	requireOK(t, s.RetainEvents(t.Context(), s.options.Clock().Add(time.Hour), s.options.Clock().Add(time.Hour)))
	assertHint(t, watch, true)
	page, err := s.Events(t.Context(), stateport.EventFilter{Repositories: []string{r.Repo}, After: 1, Limit: 500})
	requireOK(t, err)
	if !page.Gap || len(page.Events) != 0 {
		t.Fatal("pruning hid a cursor gap", page)
	}
}

func TestScopedEventQueryFiltersBeforeLimit(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "selected")
	var events []stateport.EventWrite
	for i := range 600 {
		events = append(events, stateport.EventWrite{Dedupe: fmt.Sprint("other-", i), Event: model.AgentEvent{Kind: "run.updated", Repo: r.Repo, RunID: "another"}})
	}
	events = append(events, stateport.EventWrite{Dedupe: "selected", Event: model.AgentEvent{Kind: "run.finished", Repo: r.Repo, RunID: r.ID, Ref: "refs/heads/topic"}})
	requireOK(t, s.RecordEvents(t.Context(), events))
	page, err := s.Events(t.Context(), stateport.EventFilter{RunIDs: []string{r.ID}, Kinds: []model.EventKind{model.EventRunFinished}, RefsGlob: []string{"refs/heads/*"}, Limit: 1})
	requireOK(t, err)
	if len(page.Events) != 1 || page.Events[0].RunID != r.ID || page.NextAfterID != page.Events[0].ID {
		t.Fatal("unrelated events occupied the selected page", page)
	}
	ch := s.Subscribe(stateport.ChangeScope{Runs: []string{r.ID}})
	defer ch.Close()
	requireOK(t, s.RecordEvents(t.Context(), events))
	assertHint(t, ch, false)
}
