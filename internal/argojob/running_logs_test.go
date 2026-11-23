package argojob

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRunningContainerIsTrackedBeforeTerminalCapture(t *testing.T) {
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
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "workflow", Namespace: "ci", UID: "workflow-uid"}, Status: wf.WorkflowStatus{Nodes: wf.Nodes{"node": {ID: "node", Name: "workflow.build", Type: wf.NodeTypePod, Phase: wf.NodeRunning, TemplateName: "build"}}}}
	w.Spec.Templates = []wf.Template{{Name: "build", Container: &core.Container{}}}
	p := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", Namespace: "ci", UID: "pod-uid", Labels: map[string]string{"workflows.argoproj.io/workflow": "workflow"}, Annotations: map[string]string{"workflows.argoproj.io/node-id": "node", "workflows.argoproj.io/node-name": "workflow.build"}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "workflow", UID: "workflow-uid", Controller: new(true)}}}}
	p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "main", State: core.ContainerState{Running: &core.ContainerStateRunning{}}}, {Name: "wait", State: core.ContainerState{Running: &core.ContainerStateRunning{}}}}
	tr := &tracking{binding: nativebinding.RunBinding{RunID: "run", Workflow: nativebinding.Object{Name: w.Name}}, captures: map[string]bool{}}
	e.capture(t.Context(), tr, w, []*core.Pod{p})
	entries, err := logs.Containers(t.Context(), "run")
	if err != nil || len(entries) != 2 {
		t.Fatal("live inventory absent", entries, err)
	}
	for _, entry := range entries {
		if entry.State != runlog.Following || entry.Main != (entry.Container == "main") {
			t.Fatal("live attribution lost", entry)
		}
	}
	if len(tr.pending) != 0 {
		t.Fatal("running container started a retained capture")
	}
}
