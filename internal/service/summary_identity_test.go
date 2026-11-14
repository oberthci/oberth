package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestSummaryIdentityUsesExistingPresentation(t *testing.T) {
	f := newScopeFixture(t)
	for _, run := range f.runs["github"] {
		current, err := f.store.Run(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		value, err := f.call(scopedActor, "run_get", map[string]any{"id": run.ID[:12], "summary": true})
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(value)
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		shown := f.service.presentationRun(t.Context(), current)
		for key, want := range map[string]string{"repo": "github/oberthci/oberth", "ref": run.Ref, "sha": run.SHA, "display_status": shown.DisplayStatus, "display_reason": shown.DisplayReason} {
			if got[key] != want {
				t.Errorf("%s=%v want %q", key, got[key], want)
			}
		}
	}
}

func TestPromotionResponseIdentity(t *testing.T) {
	f := newScopeFixture(t)
	p := f.promoted["github"]
	for _, terminal := range []bool{false, true} {
		if terminal {
			var err error
			p, err = f.store.FinishPromotion(t.Context(), p.ID, model.PromotionFailed, "", "failed candidate")
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, tool := range []string{"promote_status", "promotion_list"} {
			args := map[string]any{"id": p.ID[:12]}
			if tool == "promotion_list" {
				args = map[string]any{"repo": "github/oberthci/oberth"}
			}
			value, err := f.call(scopedActor, tool, args)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(value)
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if tool == "promotion_list" {
				got = got["promotions"].([]any)[0].(map[string]any)
			}
			if got["repo"] != "github/oberthci/oberth" || got["ref"] != p.SourceBranch {
				t.Fatalf("%s identity: %s", tool, encoded)
			}
		}
	}
}

type summarySchedulingProbe struct{ calls int }

func (p *summarySchedulingProbe) ObserveScheduling(context.Context, model.Run) (*model.ExecutionObservation, error) {
	p.calls++
	return nil, errors.New("summary must not query Kubernetes")
}
func TestSummaryDoesNotPollExecution(t *testing.T) {
	f := newControlFixture(t)
	run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/compact", SHA: strings.Repeat("a", 40), Actor: "reader", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	controlBefore := f.api(t)
	beforeRaw, _ := json.Marshal(map[string]any{"id": run.ID, "summary": true})
	value, err := controlBefore.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_get", beforeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if got := value.(RunSummaryResponse); got.Status != model.RunRunning {
		t.Fatalf("summary changed durable status: %+v", got)
	}
	if _, err := f.store.SetRunWorkflowName(t.Context(), run.ID, "summary-workflow"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.MarkRunSubmitted(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	control := f.api(t)
	probe := &summarySchedulingProbe{}
	control.schedulingObserver = probe
	raw, _ := json.Marshal(map[string]any{"id": run.ID, "summary": true})
	if _, err := control.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_get", raw); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 0 {
		t.Fatal("compact summary queried execution state")
	}
}
