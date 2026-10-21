package argojob

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func slice2CredentialedWorkflow(t *testing.T, kind string) (Config, *wfv1.Workflow, AdoptionRequest) {
	t.Helper()
	var config Config
	var request Request
	if kind == "promotion" {
		config = testConfigWithPerRepo()
		path := "oberth/upstream/oberthci/oberth/test-secret"
		request = testRequest(periapsis.TriggerCI, perRepoCredentialedDocument(path))
		request.UpstreamName = "codeberg"
		request.UpstreamOrg = "oberthci"
		request.Repo = "oberth"
		request.ApprovedSecrets = map[string]bool{path: true}
		request.RunTrigger = argoworkflow.RunTriggerPromotion
	} else {
		config = wifTestConfig()
		request = wifTestRequest(t, nil)
		request.RunTrigger = argoworkflow.RunTriggerTag
		request.Ref = "v1.2.3"
	}
	workflow, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	workflow.UID = "exact-credentialed-workflow"
	workflow.Status.Phase = wfv1.WorkflowSucceeded
	return config, workflow, AdoptionRequest{Name: request.Name, RunID: request.RunID, Ref: request.Ref, SHA: request.SHA, Group: "", Trigger: request.Trigger, RunTrigger: request.RunTrigger}
}

func TestAdoptedCredentialedTriggersKeepSubmittedIdentityAndWIFDespiteNewConfig(t *testing.T) {
	for _, kind := range []string{"promotion", "tag", "release"} {
		t.Run(kind, func(t *testing.T) {
			config, workflow, request := slice2CredentialedWorkflow(t, kind)
			before, err := json.Marshal(workflow.Spec)
			if err != nil {
				t.Fatal(err)
			}
			// The new owner may have new identity/policy mappings. Adoption observes
			// the already-admitted immutable spec instead of rebuilding credentials.
			config.PerRepoIdentities = map[string]PerRepoIdentityConfig{}
			config.PerRepoCIIdentities = map[string]PerRepoIdentityConfig{}
			config.ReleaseWIF = nil
			controller := &Controller{workflows: &staticWorkflowClient{workflow: workflow}, config: config, interval: time.Millisecond}
			binding, found, err := controller.Adopt(context.Background(), request)
			if err != nil || !found {
				t.Fatalf("adopt: %v %v", found, err)
			}
			completion, err := controller.WaitResumed(context.Background(), workflow.Name, request.RunID, io.Discard, nil, 0, binding)
			if err != nil || !completion.Succeeded {
				t.Fatalf("resume: %+v %v", completion, err)
			}
			after, _ := json.Marshal(workflow.Spec)
			if string(after) != string(before) {
				t.Fatal("credentialed Workflow was rewritten during adoption")
			}
			if kind == "promotion" {
				if workflow.Spec.ServiceAccountName != testPerRepoCISA {
					t.Fatal("promotion rebound to a different CI identity")
				}
			} else {
				if workflow.Annotations["oberth.ci/release-wif-policy-sha256"] == "" || !strings.Contains(string(after), "oberth-per-repo") || !strings.Contains(string(after), releaseWIFVolume) {
					t.Fatal("WIF or exact per-repo identity was lost")
				}
			}
		})
	}
}

func TestAdoptedCredentialedWaitRejectsIdentityAndWIFChanges(t *testing.T) {
	for _, mode := range []string{"service-account", "wif-projection", "wif-environment", "uid", "identity"} {
		t.Run(mode, func(t *testing.T) {
			config, workflow, request := slice2CredentialedWorkflow(t, "release")
			controller := &Controller{workflows: &staticWorkflowClient{workflow: workflow}, config: config, interval: time.Millisecond}
			binding, found, err := controller.Adopt(context.Background(), request)
			if err != nil || !found {
				t.Fatal(err)
			}
			switch mode {
			case "service-account":
				workflow.Spec.Templates[0].ServiceAccountName = "different-account"
			case "wif-projection":
				for i := range workflow.Spec.Templates[0].Volumes {
					volume := &workflow.Spec.Templates[0].Volumes[i]
					if volume.Name == releaseWIFVolume {
						volume.Projected.Sources[0].ServiceAccountToken.Audience = "different-audience"
					}
				}
			case "wif-environment":
				main := templateMainContainer(&workflow.Spec.Templates[0])
				for i := range main.Env {
					if main.Env[i].Name == "GOOGLE_APPLICATION_CREDENTIALS" {
						main.Env[i].Value = "/different/adc.json"
					}
				}
			case "uid":
				workflow.UID = "replacement-workflow"
			case "identity":
				workflow.Annotations[identityAnnotation] = strings.Repeat("f", 64)
			}
			if binding.matches(workflow) {
				t.Fatal("negative control did not modify admitted binding")
			}
			if _, err := controller.WaitResumed(context.Background(), workflow.Name, request.RunID, io.Discard, nil, 0, binding); err == nil {
				t.Fatal("changed credential binding accepted")
			}
		})
	}
}
