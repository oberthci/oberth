package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func unboundFixture(t *testing.T, s *Store) []stateport.Run {
	t.Helper()
	rejected := rejectedFixture(t, s)
	_, err := acceptForTest(t.Context(), s, rejected)
	requireOK(t, err)
	bound, _ := accepted(t, s, "bound")
	requireOK(t, s.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, runBinding(bound)))
	finished, _ := accepted(t, s, "terminal")
	requireOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: finished.ID, Revision: finished.Revision, Status: stepmodel.RunFailed}))
	first, _ := accepted(t, s, "unbound-first")
	second, _ := accepted(t, s, "unbound-second")
	source, err := s.Run(t.Context(), "source")
	requireOK(t, err)
	expected := []stateport.Run{second, first, source}
	for i, run := range expected {
		expected[i], err = s.Run(t.Context(), run.ID)
		requireOK(t, err)
	}
	return expected
}

func TestUnboundRunsPagesFullDetachedAdmissionsAcrossReopen(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	expected := unboundFixture(t, s)
	first, err := s.UnboundRuns(t.Context(), 0, 2)
	requireOK(t, err)
	if len(first) != 2 {
		t.Fatal("wrong page", first)
	}
	for i, r := range first {
		if !reflect.DeepEqual(r, expected[i]) {
			t.Fatal("incomplete or ineligible run", r, expected[i])
		}
	}
	first[0].Admission.Plan.Entries[0].Name = "changed"
	first[0].Admission.Workflow.Name = "changed"
	again, err := s.UnboundRuns(t.Context(), 0, 2)
	requireOK(t, err)
	if !reflect.DeepEqual(again[0], expected[0]) {
		t.Fatal("admission aliases reader data")
	}
	path, options := s.path, s.options
	requireOK(t, s.Close())
	reopened, err := Open(t.Context(), path, options)
	requireOK(t, err)
	defer func() { requireOK(t, reopened.Close()) }()
	rest, err := reopened.UnboundRuns(t.Context(), first[1].Sequence, 2)
	requireOK(t, err)
	if len(rest) != 1 || !reflect.DeepEqual(rest[0], expected[2]) {
		t.Fatal("cursor repeated or skipped eligible work", rest)
	}
	empty, err := reopened.UnboundRuns(t.Context(), rest[0].Sequence, 2)
	requireOK(t, err)
	if len(empty) != 0 {
		t.Fatal("page did not end", empty)
	}
}

func TestUnboundRunsBoundsAndOutage(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, _ = accepted(t, s, "unbound")
	page, err := s.UnboundRuns(t.Context(), 0, 200)
	requireOK(t, err)
	if len(page) != 1 {
		t.Fatal("valid limit lost run")
	}
	for _, bounds := range []struct {
		before int64
		limit  int
	}{{0, 201}, {0, 0}, {-1, 1}} {
		_, err := s.UnboundRuns(t.Context(), bounds.before, bounds.limit)
		requireCode(t, err, publicerr.InvalidInput)
	}
	requireOK(t, s.Close())
	_, err = s.UnboundRuns(t.Context(), 0, 1)
	requireCode(t, err, publicerr.Unavailable)
}

func TestUnboundRunsUsesSequenceIndex(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	plan := queryPlan(t, s, unboundQuery+` AND sequence<100 ORDER BY sequence DESC LIMIT 200`)
	if !strings.Contains(plan, "run_unbound") {
		t.Fatal("recovery scan lacks its cursor index", plan)
	}
	t.Log(plan)
}
