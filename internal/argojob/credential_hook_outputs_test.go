package argojob

import (
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func credentialHookOutputRequest(t *testing.T, dag, onExit, capture bool) (Config, Request) {
	t.Helper()
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
		reference := alias
		if !capture {
			reference = "ordinary-result"
		}
		parent := wfv1.Template{Name: "parent", Outputs: wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "result", ValueFrom: &wfv1.ValueFrom{Parameter: "{{" + prefix + "." + reference + ".outputs.result}}"}}}}}
		hooks := wfv1.LifecycleHooks{wfv1.LifecycleEvent(alias): wfv1.LifecycleHook{Template: leaf.Name, Expression: "true"}}
		// The ordinary invocation owns the referenced alias. The credentialed
		// leaf is invoked only through a different sibling's hook.
		if dag {
			task := wfv1.DAGTask{Name: "host", Template: "ordinary", Hooks: hooks}
			if onExit {
				task.Hooks = nil
				task.OnExit = leaf.Name
			}
			parent.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: reference, Template: "ordinary"}, task}}
		} else {
			step := wfv1.WorkflowStep{Name: "host", Template: "ordinary", Hooks: hooks}
			if onExit {
				step.Hooks = nil
				step.OnExit = leaf.Name
			}
			parent.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: reference, Template: "ordinary"}, step}}}
		}
		wf.Spec.Entrypoint = "parent"
		wf.Spec.Templates = append(wf.Spec.Templates, parent, wfv1.Template{Name: "ordinary", Container: &corev1.Container{Image: "alpine:3.20"}})
	})
	return config, request
}

func TestCredentialHookResultCaptureRefused(t *testing.T) {
	for _, dag := range []bool{false, true} {
		for _, onExit := range []bool{false, true} {
			t.Run(fmt.Sprintf("dag=%t/onExit=%t", dag, onExit), func(t *testing.T) {
				config, request := credentialHookOutputRequest(t, dag, onExit, true)
				_, err := Build(config, request)
				if err == nil || !strings.Contains(err.Error(), "referenced outputs.result") || !strings.Contains(err.Error(), "another container of the Pod") {
					t.Fatal("hook result capture was admitted", err)
				}
			})
		}
	}
}

func TestCredentialHookUnreferencedResultAllowed(t *testing.T) {
	for _, dag := range []bool{false, true} {
		for _, onExit := range []bool{false, true} {
			t.Run(fmt.Sprintf("dag=%t/onExit=%t", dag, onExit), func(t *testing.T) {
				config, request := credentialHookOutputRequest(t, dag, onExit, false)
				if _, err := Build(config, request); err != nil {
					t.Fatal("unreferenced credentialed hook was refused", err)
				}
			})
		}
	}
}
