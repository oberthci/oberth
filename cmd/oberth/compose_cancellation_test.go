//go:build linux

package main

import (
	"encoding/json"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComposedCancellationAcrossRunViews(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "running"}[running], func(t *testing.T) {
			f := composeFixture(t, composeBinary(t))
			f.prepareCommit(t)
			f.prepareAdministrator(t)
			cancel, done := f.run(t)
			defer cancel()
			f.checkStarting(t, done)
			f.waitStarted(t, done)
			f.bindClient(t)
			if out, err := f.pushResult(t, "refs/heads/cancel", "-o", "oberth.run=1"); err != nil {
				t.Fatal(out, err)
			}
			run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
			if running {
				f.startCancellationRun(t, run)
			}
			f.requestCancellation(t, run, running)
			final := f.finishCancellationRun(t, run, running)
			f.checkCancellationViews(t, final)
		})
	}
}

func (f *composedFixture) finishCancellationRun(t *testing.T, run stateport.Run, running bool) stateport.Run {
	t.Helper()
	w := f.nativeWorkflow(t, run)
	want := map[bool]wf.ShutdownStrategy{false: wf.ShutdownStrategyTerminate, true: wf.ShutdownStrategyStop}[running]
	if w.Spec.Shutdown != want {
		t.Fatal("wrong shutdown mode", w.Spec.Shutdown, want)
	}
	if running {
		f.endNative(t, run, 143)
	} else {
		w.Status.Phase = wf.WorkflowFailed
		f.updateNative(t, w)
	}
	final := f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunCancelled })
	if final.Failure == nil || final.Failure.Class != stepmodel.FailureCancelled || final.Failure.Retry.ReasonCode != "cancelled" || !strings.HasPrefix(final.DisplayReason, "cancelled by ") {
		t.Fatal("cancellation became a code failure", final)
	}
	if !running && final.StartedAt != nil {
		t.Fatal("cancelled queued run started", final.StartedAt)
	}
	return final
}

func (f *composedFixture) requestCancellation(t *testing.T, run stateport.Run, running bool) {
	t.Helper()
	if !running {
		var receipt apicontract.Receipt
		if err := f.administratorClient(t).Invoke(t.Context(), "run_cancel", apicontract.RunCancelArgs{ID: run.ID, Reason: "test cancellation"}, &receipt); err != nil {
			t.Fatal("web cancellation action failed", err)
		}
		return
	}
	app, out, diagnostic := f.revokeApplication(t)
	if code := app.run(t.Context(), []string{"run", "cancel", run.ID, "--reason", "test cancellation"}); code != 0 {
		t.Fatal(code, out.String(), diagnostic.String())
	}
}

func (f *composedFixture) startCancellationRun(t *testing.T, run stateport.Run) {
	t.Helper()
	f.deliverSource(t, run)
	w := f.nativeWorkflow(t, run)
	name := w.Name
	if path := run.Admission.Plan.Entries[0].Path; path != "" {
		name += "." + path
	}
	w.Status = wf.WorkflowStatus{Phase: wf.WorkflowRunning, Nodes: wf.Nodes{w.Name: {
		ID: w.Name, Name: name, TemplateName: "build", Type: wf.NodeTypePod, Phase: wf.NodeRunning, StartedAt: meta.NewTime(f.clock.Now()),
	}}}
	f.updateNative(t, w)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunRunning })
}

func (f *composedFixture) checkCancellationViews(t *testing.T, run stateport.Run) {
	t.Helper()
	remote := f.administratorClient(t)
	var got apicontract.RunRecord
	if err := remote.Invoke(t.Context(), "run_get", apicontract.RunGetArgs{ID: run.ID}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != run.Status || got.DisplayReason != run.DisplayReason || len(got.Steps) != 1 {
		t.Fatal("run get differs from the cancelled verdict", got)
	}
	if run.StartedAt != nil && got.Steps[0].DisplayReason != "stopped" {
		t.Fatal("killed step appears as a code failure", got.Steps)
	}
	var page apicontract.RunPage
	if err := remote.Invoke(t.Context(), "run_list", apicontract.RunListArgs{Status: "cancelled"}, &page); err != nil || len(page.Runs) != 1 || page.Runs[0].DisplayReason != got.DisplayReason {
		t.Fatal("list differs from get", page, err)
	}
	f.checkCancelledClientDisplays(t, got)
	f.checkCancelledWeb(t, got)
}

func (f *composedFixture) checkCancelledClientDisplays(t *testing.T, got apicontract.RunRecord) {
	t.Helper()
	for _, args := range [][]string{{"run", "get", got.ID}, {"run", "list", "--status", "cancelled"}} {
		app, out, diagnostic := f.revokeApplication(t)
		if code := app.run(t.Context(), args); code != 0 || !strings.Contains(out.String(), "cancelled") {
			t.Fatal("command lost cancellation", args, out.String(), diagnostic.String())
		}
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if block, ok := Display("run_get", raw); !ok || !strings.Contains(block, got.DisplayReason) || !strings.Contains(block, "cancelled") {
		t.Fatal("agent display lost cancellation", block)
	}
}

func (f *composedFixture) checkCancelledWeb(t *testing.T, got apicontract.RunRecord) {
	t.Helper()
	response, err := f.client(f.config.Listen.HTTPS, f.adminCertificate).Get("https://oberth.local/api/runs/" + got.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var web apicontract.RunRecord
	if err := json.NewDecoder(response.Body).Decode(&web); err != nil || web.Status != got.Status || web.DisplayReason != got.DisplayReason {
		t.Fatal("web run differs from get", web, err)
	}
}
