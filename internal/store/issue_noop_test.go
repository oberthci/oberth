package store

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestUnchangedIssueEditRecordsNothing(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	issue := manualIssue(t, s)
	_, before, err := s.Audit(t.Context(), 0, 100)
	requireOK(t, err)
	changed := observeChanges(t, s)
	action := stateport.Action{Actor: "operator", Events: []stateport.EventWrite{{
		Dedupe: "edit", Event: model.AgentEvent{Repo: issue.Repo, Kind: "issue.updated", At: issue.UpdatedAt},
	}}}
	patch := model.IssuePatch{ID: issue.ID, ExpectedRevision: &issue.Revision, Title: &issue.Title}
	got, err := s.UpdateIssue(t.Context(), action, patch)
	requireOK(t, err)
	stored, err := s.Issue(t.Context(), issue.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(got, issue) || !reflect.DeepEqual(stored, issue) {
		t.Fatal("unchanged edit altered the issue")
	}
	_, after, err := s.Audit(t.Context(), 0, 100)
	requireOK(t, err)
	page, err := s.Events(t.Context(), stateport.EventFilter{Limit: 100})
	requireOK(t, err)
	if after != before || len(page.Events) != 0 || page.LatestID != 0 {
		t.Fatal("unchanged edit recorded audit or events", before, after, page)
	}
	select {
	case <-changed:
		t.Fatal("unchanged edit notified a change")
	default:
	}
	patch.Title = new("updated")
	_, err = s.UpdateIssue(t.Context(), action, patch)
	requireOK(t, err)
	_, after, err = s.Audit(t.Context(), 0, 100)
	requireOK(t, err)
	page, err = s.Events(t.Context(), stateport.EventFilter{Limit: 100})
	requireOK(t, err)
	if after.ID != before.ID+1 || len(page.Events) != 1 {
		t.Fatal("changed edit did not record audit and event", before, after, page)
	}
	select {
	case <-changed:
	default:
		t.Fatal("changed edit did not notify observers")
	}
}
