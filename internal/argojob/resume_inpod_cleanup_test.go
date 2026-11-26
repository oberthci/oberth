package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type inPodFinishRecorder struct {
	reviewR2Reporter
	owner *statefake.Runs
}

func (r *inPodFinishRecorder) Finished(ctx context.Context, id string, o Observation) error {
	run, err := r.owner.Run(ctx, id)
	if err != nil {
		return err
	}
	run.Status = o.Status
	r.owner.SeedRun(run)
	return nil
}

type refusedResumeKeep struct{}

func (refusedResumeKeep) Keep(context.Context, string, []sourcecontract.Binding) error {
	return errors.New("source retention failed")
}

func TestInPodStoppedAndUnkeptIntentReleasesOriginalHold(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"stop before relabel", "retention at apply", "retention before apply", "already terminal", "replaced Pod"} {
		t.Run(point, func(t *testing.T) { checkAbortedInPodIntent(t, point) })
	}
}

func checkAbortedInPodIntent(t *testing.T, point string) {
	t.Helper()
	config, r, plan, native, pods := inPodRecovery(t)
	owner := config.Runs.(*statefake.Runs)
	var err error
	r.Origin.ResumePlan, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if point == "already terminal" {
		r.Status = stepmodel.RunFailed
	}
	owner.SeedRun(r)
	config.Reporter = &inPodFinishRecorder{owner: owner}
	if point == "stop before relabel" {
		_, err = owner.Cancel(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
		if err != nil {
			t.Fatal(err)
		}
	}
	if point == "retention at apply" {
		config.Keeper = refusedResumeKeep{}
	}
	if point == "replaced Pod" {
		pods.pod.UID = "replacement"
	}
	e := New(config)
	defer e.queue.ShutDown()
	defer e.collector.queue.ShutDown()
	if point == "retention before apply" || point == "replaced Pod" {
		_ = e.ResumeFailed(t.Context(), r.ID, errors.New("source retention failed"))
	} else {
		_ = e.ApplyResume(t.Context(), r.ID)
	}
	assertAbortedHold(t, pods, native, point)
	current, err := owner.Run(t.Context(), r.ID)
	if err != nil || !terminalChild(current) {
		t.Fatal("aborted successor stayed active", current.Status, err)
	}
}

func assertAbortedHold(t *testing.T, pods *inPodTransport, native *inPodNative, point string) {
	t.Helper()
	want := 1
	if point == "replaced Pod" {
		want = 0
	}
	if pods.runs != 0 || pods.releases != want || native.patches != 0 {
		t.Fatal("aborted intent leaked or reran hold", point, pods.runs, pods.releases, native.patches)
	}
}
