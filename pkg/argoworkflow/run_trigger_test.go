package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func decodeAdmissible(t *testing.T, annotations map[string]string) *wfv1.Workflow {
	t.Helper()
	workflow, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	workflow.Annotations = annotations
	return workflow
}

const triggerGuardedDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  arguments:
    parameters:
    - name: other
      value: kept
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

func TestReservedRunParameterDeclarations(t *testing.T) {
	mustAdmit(t, triggerGuardedDocument, Policy{})
	for _, name := range []string{"oberth-repo", "oberth-ref", "oberth-sha", TriggerParameter, "oberth-release-tag", "oberth-release-sha", ScheduleParameter, "oberth-future"} {
		for _, shape := range []string{"", "\n      value: promotion", "\n      default: promotion", "\n      value: promotion\n      enum: [promotion]", "\n      valueFrom:\n        supplied: {}"} {
			t.Run(name+shape, func(t *testing.T) {
				document := strings.Replace(triggerGuardedDocument, "    - name: other\n      value: kept", "    - name: "+name+shape, 1)
				refuse(t, document, Policy{}, "server-owned")
			})
		}
	}
}

func TestReservedParameterPrefixInNestedScopes(t *testing.T) {
	for _, section := range []string{"inputs", "outputs"} {
		document := strings.Replace(triggerGuardedDocument, "    - name: leaf\n", "    - name: leaf\n      "+section+":\n        parameters:\n        - name: oberth-sha\n          value: forged\n", 1)
		refuse(t, document, Policy{}, "server-owned")
	}
	document := strings.Replace(triggerGuardedDocument, "        - name: fast\n          template: leaf", "        - name: fast\n          template: leaf\n          arguments:\n            parameters:\n            - name: oberth-future\n              value: forged", 1)
	refuse(t, document, Policy{}, "server-owned")
}

func TestSetTriggerParameterStampsEveryWorkflow(t *testing.T) {
	for _, document := range []string{triggerGuardedDocument, admissibleDocument} {
		workflow, err := Decode([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		for _, trigger := range []RunTrigger{RunTriggerBranch, RunTriggerSchedule} {
			if err := SetTriggerParameter(workflow, trigger); err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, p := range workflow.Spec.Arguments.Parameters {
				if p.Name == TriggerParameter {
					found++
					if p.Value == nil || p.Value.String() != string(trigger) {
						t.Fatalf("trigger = %+v", p)
					}
				} else if p.Name != "other" || p.Value.String() != "kept" {
					t.Fatalf("authored parameter changed: %+v", p)
				}
			}
			if found != 1 {
				t.Fatalf("trigger parameter count = %d", found)
			}
		}
	}
	if err := SetTriggerParameter(nil, RunTriggerBranch); err == nil {
		t.Fatal("nil Workflow accepted")
	}
	if err := SetTriggerParameter(decodeAdmissible(t, nil), RunTrigger("push")); err == nil {
		t.Fatal("an unknown run trigger was stamped")
	}
}

func TestReservedGlobalParameterPrefix(t *testing.T) {
	for _, global := range []string{"oberth-sha", "oberth-future"} {
		document := strings.Replace(triggerGuardedDocument, "    - name: leaf\n", "    - name: leaf\n      outputs:\n        parameters:\n        - name: result\n          value: forged\n          globalName: "+global+"\n", 1)
		refuse(t, document, Policy{}, "globalName is server-owned")
	}
	document := strings.Replace(triggerGuardedDocument, "    - name: leaf\n", "    - name: leaf\n      outputs:\n        parameters:\n        - name: result\n          value: kept\n          globalName: user-result\n", 1)
	mustAdmit(t, document, Policy{})
}
