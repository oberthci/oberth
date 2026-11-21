package argojob_test

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/stateport"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
)

func conditionalDocument(t *testing.T, tc conditionalEvidenceCase) []byte {
	t.Helper()
	w := wf.MustUnmarshalWorkflow(conditionalWorkflow)
	tasks := w.Spec.Templates[0].DAG.Tasks
	if tc.change != "other-failed" {
		tasks = tasks[:1]
	} else {
		tasks[1].When = ""
	}
	if tc.capture != "when-skipped" {
		tasks[0].When = ""
	}
	if tc.capture == "omitted" {
		before := *w.Spec.Templates[1].DeepCopy()
		before.Name = "before"
		before.Container.ReadinessProbe = &core.Probe{ProbeHandler: core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"true"}}}}
		w.Spec.Templates = append(w.Spec.Templates, before)
		tasks[0].Dependencies = []string{"before"}
		tasks = append(tasks, wf.DAGTask{Name: "before", Template: "before"})
	}
	w.Spec.Templates[0].DAG.Tasks = tasks
	if strings.HasPrefix(tc.capture, "restart-") {
		w.Spec.Entrypoint = "build"
	}
	raw, err := json.Marshal(w)
	compositionOK(t, err)
	return raw
}

func addConditionalFailure(t *testing.T, w *wf.Workflow) {
	t.Helper()
	failed := conditionalStatus(t, "failed", w.Name)
	for _, other := range failed.Nodes {
		if other.TemplateName != "build" {
			continue
		}
		other.ID, other.Name, other.DisplayName = w.Name+"-other", w.Name+".other", "other"
		w.Status.Nodes[other.ID] = other
		root := w.Status.Nodes[w.Name]
		root.Children = append(root.Children, other.ID)
		w.Status.Nodes[root.ID] = root
	}
}

func assertNeverRunSkipNode(t *testing.T, node stateport.Node) {
	t.Helper()
	if !node.NeverRunSkip || node.PodName != "" || node.Exit != nil {
		t.Fatal("positive skip was not durably recorded", node)
	}
}
