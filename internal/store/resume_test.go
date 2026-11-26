package store

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func resumeAcceptance(t *testing.T, s *Store) (stateport.Run, stateport.Acceptance) {
	t.Helper()
	r, input := accepted(t, s, "predecessor")
	requireOK(t, s.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, runBinding(r)))
	r, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	requireOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed}))
	r, err = s.Run(t.Context(), r.ID)
	requireOK(t, err)
	input = reacceptance(t, input, "successor")
	plan := nativebinding.ResumePlan{UID: "uid", ResourceVersion: "7", Workflow: &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: r.Origin.WorkflowName, Labels: map[string]string{nativebinding.RunLabel: "successor"}}}}
	body, err := json.Marshal(plan)
	requireOK(t, err)
	input.Resume = &model.RunOrigin{ResumedFrom: r.ID, ResumeUID: r.Origin.NativeUID(), WorkflowName: r.Origin.WorkflowName, Assisted: true, ResumePlan: body}
	return r, input
}

func TestResumeSuccessorSerialized(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	before, input := resumeAcceptance(t, s)
	other := reacceptance(t, input, "other-successor")
	origin := *input.Resume
	other.Resume = &origin
	var plan nativebinding.ResumePlan
	requireOK(t, json.Unmarshal(origin.ResumePlan, &plan))
	plan.Workflow.Labels[nativebinding.RunLabel] = other.Prepared.View().RunID
	var err error
	other.Resume.ResumePlan, err = json.Marshal(plan)
	requireOK(t, err)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, a := range []stateport.Acceptance{input, other} {
		wg.Go(func() { <-start; _, err := acceptForTest(t.Context(), s, a); results <- err })
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded, failed := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else {
			failed++
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatal("two resumes were not serialized", succeeded, failed)
	}
	after, err := s.Run(t.Context(), before.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("resume changed predecessor")
	}
	next, err := s.ResumeSuccessor(t.Context(), before.ID)
	requireOK(t, err)
	row, err := s.Run(t.Context(), next)
	requireOK(t, err)
	if !row.Origin.Assisted || row.Origin.WorkflowUID != "" || row.Origin.NativeUID() != before.Origin.NativeUID() {
		t.Fatal("resume identity", row.Origin)
	}
	rows, err := s.UnboundRuns(t.Context(), 0, 200)
	requireOK(t, err)
	for _, r := range rows {
		if r.ID == next {
			t.Fatal("ordinary unbound recovery owns resume")
		}
	}
}

func TestRollbackFailsResumedRunClosed(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	old, input := resumeAcceptance(t, s)
	r, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	// An earlier decoder ignores resume fields and sees an unbound record.
	body, err := json.Marshal(r.Origin)
	requireOK(t, err)
	var earlier struct {
		WorkflowName string `json:"workflow_name"`
		WorkflowUID  string `json:"workflow_uid"`
	}
	requireOK(t, json.Unmarshal(body, &earlier))
	if earlier.WorkflowUID != "" {
		t.Fatal("downgrade treats a resume as ordinary evidence")
	}
	if r.Origin.WorkflowName != old.Origin.WorkflowName || nativebinding.WorkflowName(r.ID) == old.Origin.WorkflowName {
		t.Fatal("downgrade orphan path changed")
	}
	// The old lookup uses a fresh name for R2; it cannot collect R1's relabelled
	// Workflow. The downgrade guide requires finishing or cancelling it first.
	if r.Origin.WorkflowUID != "" || !r.Origin.HasAssistance() {
		t.Fatal("resume can supply evidence")
	}
}

func TestInspectedStateNeverReachesSecretStep(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	old, input := resumeAcceptance(t, s)
	requireOK(t, s.change(t.Context(), mutation{"operator", "run.assist", "run", old.ID, stateport.HoldAudit{Kind: "exec"}}, func(*sql.Tx) error { return nil }))
	var plan nativebinding.ResumePlan
	requireOK(t, json.Unmarshal(input.Resume.ResumePlan, &plan))
	plan.SecretSteps = []string{"publish"}
	var err error
	input.Resume.ResumePlan, err = json.Marshal(plan)
	requireOK(t, err)
	if _, err = acceptForTest(t.Context(), s, input); err == nil {
		t.Fatal("inspection reached a secret step")
	}
	plan.SecretSteps = nil
	input.Resume.ResumePlan, err = json.Marshal(plan)
	requireOK(t, err)
	if _, err = acceptForTest(t.Context(), s, input); err != nil {
		t.Fatal("secret-free control refused", err)
	}
}
