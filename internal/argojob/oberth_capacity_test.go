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
				for _, task := range template.DAG.Tasks {
					if _, ok := guarded[task.Name]; !ok {
						continue
					}
					for _, dependency := range strings.Split(task.Depends, " && ") {
						if dependency == scan {
							guarded[task.Name] = true
						}
					}
				}
				for task, present := range guarded {
					if !present {
						t.Errorf("%s must retain an enforcing dependency on %s", task, scan)
					}
				}
			}
			// Bound the largest possible pair of main containers, not an
			// average usage estimate or a promise about other workflows.
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
