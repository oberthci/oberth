package app

import (
	"context"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type heldReplyService struct{ Service }

func (heldReplyService) Accept(context.Context, stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	return stateport.AcceptanceResult{Runs: []stateport.Run{{RunSummary: stepmodel.RunSummary{ID: "next"}}}}, nil
}

type heldReplyKeeper struct{}

func (heldReplyKeeper) Keep(context.Context, string, []sourcecontract.Binding) error { return nil }

type heldReplyNative struct{ resumeNative }

func (heldReplyNative) ApplyResume(context.Context, string) error { return nil }

func TestInPodResumeReplyNamesKeptStateAndDeadline(t *testing.T) {
	t.Parallel()
	s, accepted := heldReplyFixture(t)
	for _, near := range []bool{false, true} {
		left, advice := int64(-1), ""
		if near {
			left, advice = 10000, "retry"
		}
		plan := nativebinding.ResumePlan{InPod: &nativebinding.ResumeHold{DeadlineRemainingMS: left, RetryAdvised: near}}
		out, err := s.acceptResume(t.Context(), accepted, plan, heldReplyNative{})
		if err != nil || out.Run != "next" || out.Mode != "in-pod" || out.DeadlineRemainingMS == nil || *out.DeadlineRemainingMS != left || out.Advice != advice {
			t.Fatal("in-Pod reply lost deadline advice", out, err)
		}
		for _, phrase := range []string{"untracked files named in held-step logs", "inspection state", "processes kept", "secret-free remaining steps"} {
			if !strings.Contains(out.Detail, phrase) {
				t.Fatal("in-Pod reply hides kept workspace state", out.Detail, phrase)
			}
		}
	}
}

func heldReplyFixture(t *testing.T) (*Submitter, stateport.Acceptance) {
	t.Helper()
	prepared, err := admissioncontract.Seal(admissioncontract.PreparedView{RunID: "next", Workflow: &wf.Workflow{}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Submitter{d: Deps{Service: heldReplyService{}, Keeper: heldReplyKeeper{}, Deadline: func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return context.WithCancel(ctx)
	}}}
	return s, stateport.Acceptance{Prepared: prepared, Request: model.CIRequest{ObjectSHA: strings.Repeat("f", 40)}, Resume: &model.RunOrigin{ResumedFrom: "previous"}}
}
