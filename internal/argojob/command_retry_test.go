package argojob_test

import (
	"encoding/json"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

func TestCommandFailureExplainsRetryUncertainty(t *testing.T) {
	for _, message := range []string{"Get https://proxy.golang.org/module: net/http: TLS handshake timeout", "test assertion failed", ""} {
		t.Run(message, func(t *testing.T) {
			w := nativeWorkflow()
			w.Status.Phase = wf.WorkflowFailed
			n := nativeNode("lint", ".lint", wf.NodeFailed, 1)
			w.Status.Nodes[n.ID] = n
			p := nativePod(w, n, 1)
			p.Status.ContainerStatuses[0].State.Terminated.Message = message
			out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "lint", Name: "lint"}}}, Workflow: w, Pods: []*core.Pod{p}})
			if out.Failure == nil || out.Failure.Class != stepmodel.FailureAuthor || out.Failure.Retry.Action != stepmodel.RetryNone {
				t.Fatalf("command evidence changed: %+v", out.Failure)
			}
			hint, err := json.Marshal(out.Failure.Retry)
			for _, want := range []string{"\"detail\":", "cannot tell whether rerunning unchanged is safe", "workflow YAML governs retries"} {
				if err != nil || !strings.Contains(string(hint), want) {
					t.Fatalf("retry uncertainty missing: %s %v", hint, err)
				}
			}
		})
	}
}
