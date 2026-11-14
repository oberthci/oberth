package argojob

import (
	"fmt"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"strings"
	"testing"
)

func TestCredentialHookResultCaptureRefused(t *testing.T) {
	for _, dag := range []bool{false, true} {
		for _, onExit := range []bool{false, true} {
			t.Run(fmt.Sprintf("dag=%t/onExit=%t", dag, onExit), func(t *testing.T) {
				config, request := credentialOutputRequest(t, false, false, "")
				request = changeCredentialOutputRequest(t, request, func(wf *wfv1.Workflow) {
					leaf := &wf.Spec.Templates[0]
					leaf.Outputs = wfv1.Outputs{}
					credentialOtherContainer(leaf, "sidecar")
					alias, prefix := "scan", "steps"
					if onExit {
						alias = "onExit"
					}
					if dag {
						prefix = "tasks"
					}
					parent := wfv1.Template{Name: "parent", Outputs: wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Parameter: "{{" + prefix + "." + alias + ".outputs.result}}"}}}}}
					hooks := wfv1.LifecycleHooks{wfv1.LifecycleEvent(alias): wfv1.LifecycleHook{Template: leaf.Name, Expression: "true"}}
					if dag {
						task := wfv1.DAGTask{Name: alias, Template: "ordinary", Hooks: hooks}
						if onExit {
							task.Hooks = nil
							task.OnExit = leaf.Name
						}
						parent.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{task}}
					} else {
						step := wfv1.WorkflowStep{Name: alias, Template: "ordinary", Hooks: hooks}
						if onExit {
							step.Hooks = nil
							step.OnExit = leaf.Name
						}
						parent.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{step}}}
					}
					wf.Spec.Entrypoint = "parent"
					wf.Spec.Templates = append(wf.Spec.Templates, parent, wfv1.Template{Name: "ordinary", Container: &corev1.Container{Image: "alpine:3.20"}})
				})
				_, err := Build(config, request)
				if err == nil || !strings.Contains(err.Error(), "referenced outputs.result") || !strings.Contains(err.Error(), "another container of the Pod") {
					t.Fatal("hook result capture was admitted", err)
				}
			})
		}
	}
}
