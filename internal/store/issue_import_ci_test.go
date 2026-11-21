package store

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestIssueImportManualCopiesAreSeparateFromNativeCI(t *testing.T) {
	s := testStore(t)
	red, _ := accepted(t, s, "import-red")
	q := importedBatch()
	q.Records = q.Records[:1]
	v := &q.Records[0]
	v.Kind, v.Repo, v.Branch, v.Upstream = model.IssueManual, red.Repo, red.Ref, "origin"
	v.ID, v.Body = 50, "source body one"
	second := *v
	second.ID, second.Body = 90, "source body two"
	q.Records = append(q.Records, second)
	_, err := s.ImportIssues(t.Context(), importAdmin(), q)
	requireOK(t, err)
	observer := stateport.Action{Actor: "observer"}
	requireOK(t, s.Finish(t.Context(), observer, stateport.RunUpdate{ID: red.ID, Revision: red.Revision, Status: stepmodel.RunFailed}))
	requireOK(t, s.ProjectCI(t.Context(), observer, red.ID))
	got, err := s.Issue(t.Context(), v.ID)
	requireOK(t, err)
	if got.Kind != model.IssueManual || got.Body != v.Body || got.CIWorkID != "" || !sameImportHistory(got.History, v.History) {
		t.Fatal("native red changed a source copy")
	}
	green, _ := accepted(t, s, "import-green")
	requireOK(t, s.Finish(t.Context(), observer, stateport.RunUpdate{ID: green.ID, Revision: green.Revision, Status: stepmodel.RunPassed}))
	requireOK(t, s.ProjectCI(t.Context(), observer, green.ID))
	got, err = s.Issue(t.Context(), v.ID)
	requireOK(t, err)
	if got.Body != v.Body || got.CIWorkID != "" || got.Closer != v.Closer {
		t.Fatal("native green lost source attribution")
	}
	replay, err := s.ImportIssues(t.Context(), importAdmin(), q)
	requireOK(t, err)
	if !replay.AlreadyImported {
		t.Fatal("native projection destroyed replay")
	}
	checkNativeCIImportResult(t, s, green.ID)
}

func checkNativeCIImportResult(t *testing.T, s *Store, runID string) {
	rows, err := s.Issues(t.Context(), nil, model.IssueFilter{Limit: 100, State: "all"})
	requireOK(t, err)
	if len(rows) != 3 || rows[0].Kind != model.IssueCI || rows[0].ID <= 90 || rows[0].CIWorkID != runID || rows[0].State != model.IssueClosed {
		t.Fatal("native projection did not create its own issue", rows)
	}
}

func sameImportHistory(a, b []model.IssueHistory) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
