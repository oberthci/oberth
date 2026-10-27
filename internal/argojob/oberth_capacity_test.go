package argojob

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestOberthWorkflowCapacityAndSecurityOrdering(t *testing.T) {
	for _, name := range []string{"build.yaml", "release.yaml"} {
		t.Run(name, func(t *testing.T) {
			workflow, err := argoworkflow.Decode(loadPipeline(t, name))
			if err != nil {
				t.Fatal(err)
			}
			if workflow.Spec.Parallelism == nil || *workflow.Spec.Parallelism != 2 {
				t.Fatal("the repository workflow must bound its own Pod fan-out to two")
			}
			var requests []int64
			for _, template := range workflow.Spec.Templates {
				if template.Container != nil {
					requests = append(requests, template.Container.Resources.Requests.Cpu().MilliValue())
				}
				if template.Script != nil {
					requests = append(requests, template.Script.Resources.Requests.Cpu().MilliValue())
				}
				if template.Name != workflow.Spec.Entrypoint {
					continue
				}
				scan := "security"
				guarded := map[string]bool{"lint": false, "test-openbao": false, "release-diagnostic": false, "verify-publisher-tools": false}
				if name == "release.yaml" {
					scan = "release-scan"
					guarded = map[string]bool{"release-lint": false, "release-chart-test": false, "release-verify-publisher-tools": false}
				}
				scanRequired := false
				for _, task := range template.DAG.Tasks {
					if task.Name == scan {
						scanRequired = task.When == "" && task.ContinueOn == nil && task.Depends == map[string]string{
							"build.yaml": "setup", "release.yaml": "release-setup",
						}[name]
					}
					if _, ok := guarded[task.Name]; !ok {
						continue
					}
					if name == "build.yaml" {
						guarded[task.Name] = task.Depends == "setup"
						continue
					}
					for _, dependency := range strings.Split(task.Depends, " && ") {
						if dependency == scan {
							guarded[task.Name] = true
						}
					}
				}
				if !scanRequired {
					t.Fatal("the security scan must remain unconditional and enforcing after setup")
				}
				for task, present := range guarded {
					if !present {
						t.Errorf("%s has incorrect security ordering for %s", task, name)
					}
				}
			}
			// This bounds only the declared main-container pair. Argo wait/init
			// containers and Pod overhead are excluded; it is not a whole-Pod
			// request or a guarantee of free node CPU for other workflows.
			var largest, second int64
			for _, request := range requests {
				if request >= largest {
					second, largest = largest, request
				} else if request > second {
					second = request
				}
			}
			if largest == 0 || largest+second > 8000 {
				t.Fatalf("main-container pair requests %dm CPU; exceeds the reviewed 8000m bound", largest+second)
			}
		})
	}
}
