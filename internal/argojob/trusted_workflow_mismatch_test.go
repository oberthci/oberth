package argojob

import (
	"encoding/json"
	"errors"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestTrustedWorkflowMismatchOriginalObserverPath(t *testing.T) {
	for _, scenario := range []string{"quantity serialization", "owned mismatch", "foreign run", "foreign source"} {
		t.Run(scenario, func(t *testing.T) {
			config, request, intended, _ := trustedKVMBuilt(t)
			body, err := json.Marshal(intended)
			if err != nil {
				t.Fatal(err)
			}
			var actual wfv1.Workflow
			if err = json.Unmarshal(body, &actual); err != nil {
				t.Fatal(err)
			}
			for i := range actual.Spec.Templates {
				if c := actual.Spec.Templates[i].Container; c != nil {
					for name, q := range c.Resources.Requests {
						q.AsDec()
						c.Resources.Requests[name] = q
					}
					for name, q := range c.Resources.Limits {
						q.AsDec()
						c.Resources.Limits[name] = q
					}
				}
			}
			switch scenario {
			case "owned mismatch":
				actual.Spec.ServiceAccountName = "altered"
			case "foreign run":
				actual.Annotations[runIDAnnotation] = "another-run"
			case "foreign source":
				actual.Annotations[identityAnnotation] = "another-source"
			}
			controller := &Controller{config: config, workflows: &staticWorkflowClient{workflow: &actual}, kube: trustedControllerClient()}
			uid, err := controller.TrustedKVMWorkflowUID(t.Context(), request)
			var mismatch interface{ WorkflowSpecMismatch() bool }
			if scenario == "quantity serialization" {
				if err != nil || uid != string(actual.UID) {
					t.Fatalf("serialized admitted spec refused: %s %v", uid, err)
				}
				return
			}
			if err == nil {
				t.Fatal("altered or foreign Workflow accepted")
			}
			classified := errors.As(err, &mismatch) && mismatch.WorkflowSpecMismatch()
			if classified != (scenario == "owned mismatch") {
				t.Fatalf("cleanup classification for %s: %v", scenario, err)
			}
		})
	}
}
