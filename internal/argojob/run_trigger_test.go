package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// guardedDocument skips its heavy task on branch runs and declares a
// promotion default for servers that predate the parameter.
const guardedDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: XL
    oberth.ci/size-by-trigger: branch=M,promotion=XL,schedule=XL
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  arguments:
    parameters:
    - name: oberth-trigger
      value: promotion
  templates:
    - name: main
      dag:
        tasks:
        - name: fast
          template: leaf
        - name: matrix
          template: leaf
          when: "'{{workflow.parameters.oberth-trigger}}' != 'branch'"
    - name: leaf
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`

func triggerParameters(workflow *wfv1.Workflow) []string {
	var values []string
	for _, parameter := range workflow.Spec.Arguments.Parameters {
		if parameter.Name == argoworkflow.TriggerParameter {
			values = append(values, parameter.Value.String())
		}
	}
	return values
}

func TestBuildStampsTheDurableRunTriggerOverTheDocumentDefault(t *testing.T) {
	for _, trigger := range []argoworkflow.RunTrigger{
		argoworkflow.RunTriggerBranch, argoworkflow.RunTriggerPromotion, argoworkflow.RunTriggerSchedule,
	} {
		request := testRequest(periapsis.TriggerCI, guardedDocument)
		request.RunTrigger = trigger
		built, err := Build(testConfig(), request)
		if err != nil {
			t.Fatalf("build %s: %v", trigger, err)
		}
		if values := triggerParameters(built); len(values) != 1 || values[0] != string(trigger) {
			t.Fatalf("%s run submitted oberth-trigger %v", trigger, values)
		}
	}

	// A release run of a document that uses the parameter is a tag run.
	release := strings.Replace(guardedDocument, "branch=M,promotion=XL,schedule=XL", "tag=L", 1)
	built, err := Build(testConfig(), testRequest(periapsis.TriggerRelease, release))
	if err != nil {
		t.Fatalf("build release: %v", err)
	}
	if values := triggerParameters(built); len(values) != 1 || values[0] != "tag" {
		t.Fatalf("release run submitted oberth-trigger %v", values)
	}
}

func TestBuildLeavesDocumentsThatNeverMentionTheParameterUnchanged(t *testing.T) {
	request := testRequest(periapsis.TriggerCI, greedyDocument)
	request.RunTrigger = argoworkflow.RunTriggerPromotion
	built, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(built.Spec.Arguments.Parameters) != 0 {
		t.Fatalf("unrelated document gained parameters %+v", built.Spec.Arguments.Parameters)
	}
}

func TestBuildRefusesARunTriggerOutsideItsTier(t *testing.T) {
	for _, testCase := range []struct {
		tier    periapsis.Trigger
		trigger argoworkflow.RunTrigger
	}{
		{periapsis.TriggerCI, argoworkflow.RunTriggerTag},
		{periapsis.TriggerRelease, argoworkflow.RunTriggerBranch},
		{periapsis.TriggerRelease, argoworkflow.RunTriggerPromotion},
		{periapsis.TriggerCI, argoworkflow.RunTrigger("push")},
	} {
		request := testRequest(testCase.tier, guardedDocument)
		request.RunTrigger = testCase.trigger
		if _, err := Build(testConfig(), request); err == nil || !strings.Contains(err.Error(), "run trigger") {
			t.Fatalf("%s run under %s tier: %v", testCase.trigger, testCase.tier, err)
		}
	}
}
