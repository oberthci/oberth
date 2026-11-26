package store

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestInPodAncestryNeverReachesSecretRetry(t *testing.T) {
	t.Parallel()
	for _, interveningRetry := range []bool{false, true} {
		s := testStore(t)
		_, input := inPodAcceptance(t, s)
		previous, err := acceptForTest(t.Context(), s, input)
		requireOK(t, err)
		if interveningRetry {
			input = heldRetryAcceptance(t, s, previous, input)
			previous, err = acceptForTest(t.Context(), s, input)
			requireOK(t, err)
		}
		input = heldRetryAcceptance(t, s, previous, input)
		var plan nativebinding.ResumePlan
		requireOK(t, json.Unmarshal(input.Resume.ResumePlan, &plan))
		plan.SecretSteps = []string{"publish"}
		input.Resume.ResumePlan, err = json.Marshal(plan)
		requireOK(t, err)
		if _, err = acceptForTest(t.Context(), s, input); err == nil {
			t.Fatal("in-Pod ancestry reached a secret retry", interveningRetry)
		}
		plan.SecretSteps = nil
		input.Resume.ResumePlan, err = json.Marshal(plan)
		requireOK(t, err)
		_, err = acceptForTest(t.Context(), s, input)
		requireOK(t, err)
	}
}

func heldRetryAcceptance(t *testing.T, s *Store, previous stateport.Run, input stateport.Acceptance) stateport.Acceptance {
	t.Helper()
	requireOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: previous.ID, Revision: previous.Revision, Status: stepmodel.RunFailed}))
	id := previous.ID + "-retry"
	input = reacceptance(t, input, id)
	var plan nativebinding.ResumePlan
	requireOK(t, json.Unmarshal(input.Resume.ResumePlan, &plan))
	plan.InPod, plan.SecretSteps = nil, nil
	plan.Workflow.Labels[nativebinding.RunLabel] = id
	body, err := json.Marshal(plan)
	requireOK(t, err)
	input.Resume = &model.RunOrigin{Assisted: true, ResumedFrom: previous.ID, ResumeUID: previous.Origin.NativeUID(), WorkflowName: previous.Origin.WorkflowName, ResumePlan: body}
	return input
}
