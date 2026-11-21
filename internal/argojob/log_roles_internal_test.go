package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestCaptureMarksAuthoredContainersAndDiagnosesWaitFailure(t *testing.T) {
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := logs.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	e := New(Config{Logs: logs})
	defer e.queue.ShutDown()
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "w", Namespace: "ci", UID: "workflow"}, Status: wf.WorkflowStatus{Nodes: wf.Nodes{"n": {ID: "n", Name: "w.build", Type: wf.NodeTypePod, Phase: wf.NodeFailed, TemplateName: "build"}}}}
	w.Spec.Templates = []wf.Template{{Name: "build", ContainerSet: &wf.ContainerSetTemplate{Containers: []wf.ContainerNode{{Container: core.Container{Name: "compile"}}, {Container: core.Container{Name: "test"}}}}}}
	p := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", Namespace: "ci", UID: "pod", Labels: map[string]string{"workflows.argoproj.io/workflow": "w"}, Annotations: map[string]string{"workflows.argoproj.io/node-id": "n", "workflows.argoproj.io/node-name": "w.build"}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "w", UID: "workflow", Controller: new(true)}}}}
	ended := func(name string, code int32) core.ContainerStatus {
		return core.ContainerStatus{Name: name, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: code}}}
	}
	p.Status.InitContainerStatuses = []core.ContainerStatus{ended("init", 0)}
	p.Status.ContainerStatuses = []core.ContainerStatus{ended("compile", 0), ended("test", 0), ended("wait", 1)}
	tr := &tracking{binding: nativebinding.RunBinding{Workflow: nativebinding.Object{Name: w.Name}}, captures: map[string]bool{}}
	e.tracked[w.Name] = tr
	e.capture(tr, w, []*core.Pod{p})
	entries := tr.pending["build"].entries
	if len(entries) != 4 {
		t.Fatal(entries)
	}
	for _, entry := range entries {
		want := entry.Container == "compile" || entry.Container == "test"
		if entry.Main != want {
			t.Fatal("wrong authored role", entry)
		}
	}
	if d := diagnose(w.Status.Nodes["n"], p, false, &w.Spec.Templates[0]); d.container != "wait" || d.class != stepmodel.FailureUnknown {
		t.Fatal("executor failure hidden", d)
	}
	p.Status.ContainerStatuses = []core.ContainerStatus{ended("main", 0), ended("sidecar", 0), ended("wait", 0)}
	w.Spec.Templates[0] = wf.Template{Name: "build", Container: &core.Container{}}
	if own := authoredContainerStatuses(&w.Spec.Templates[0], p); len(own) != 1 || own[0].Name != "main" {
		t.Fatal("sidecar became authored output", own)
	}
}
