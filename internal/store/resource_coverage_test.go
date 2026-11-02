package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestResourceCoveragePersistsInExistingJSONAndLegacyStaysUnknown(t *testing.T) {
	at := time.Now().UTC()
	s := testStore(t, &at)
	repo := createRepo(t, s)
	var ids []string
	for i := range 3 {
		run, e := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "main", strings.Repeat(string(rune('a'+i)), 40)))
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.SetRunJobName(t.Context(), run.ID, "step-abc12345"); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, run.ID)
		timing := model.WorkflowTiming{RunID: run.ID, Namespace: "oberth", Name: "step-abc12345", UID: "11111111-1111-4111-8111-111111111111", SpecIdentity: strings.Repeat("a", 64), CreatedAt: at, Tasks: []model.TimingTask{}}
		if i == 0 {
			timing.Resources = &model.ResourceCoverage{Declared: 2, Undeclared: 1, Fallback: model.ResourceFallbackBudget}
		}
		if i == 2 {
			timing.Resources = &model.ResourceCoverage{Unknown: true, Fallback: model.ResourceFallbackGraph}
		}
		if e = s.RecordWorkflowTiming(t.Context(), timing); e != nil {
			t.Fatal(e)
		}
		if i == 1 {
			timing.Resources = &model.ResourceCoverage{Undeclared: 1}
			if e = s.RecordWorkflowTiming(t.Context(), timing); e != nil {
				t.Fatal("legacy observation rejected", e)
			}
		}
	}
	got, e := s.ResourceCoverages(t.Context(), ids)
	if e != nil || len(got) != 2 || got[ids[0]].Undeclared != 1 {
		t.Fatalf("coverage: %+v %v", got, e)
	}
	if _, ok := got[ids[1]]; ok {
		t.Fatal("legacy timing rewritten")
	}
	if got[ids[0]].Fallback != model.ResourceFallbackBudget || !strings.Contains(got[ids[0]].Message(ids[0]), "legacy size") {
		t.Fatal("completed fallback notice was not retained")
	}
	if c := got[ids[2]]; !c.Unknown || c.Fallback != model.ResourceFallbackGraph || !strings.Contains(c.Message(ids[2]), "cannot be bounded") {
		t.Fatal("unknown graph lost its durable fallback notice")
	}
	if _, e := s.ResourceCoverages(t.Context(), make([]string, 513)); e == nil {
		t.Fatal("unbounded coverage read")
	}
}
