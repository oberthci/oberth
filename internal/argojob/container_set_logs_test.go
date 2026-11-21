package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"io"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
	"testing"
)

func TestContainerSetWithMainKeepsAllAuthoredOutput(t *testing.T) {
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logs.Close() }()
	template := wf.Template{Name: "build", ContainerSet: &wf.ContainerSetTemplate{Containers: []wf.ContainerNode{{Container: core.Container{Name: "main"}}, {Container: core.Container{Name: "test"}}}}, Sidecars: []wf.UserContainer{{Container: core.Container{Name: "sidecar"}}}}
	n := wf.NodeStatus{ID: "n", Name: "w.build", Type: wf.NodeTypePod, Phase: wf.NodeFailed, TemplateName: template.Name}
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "w", Namespace: "ci", UID: "workflow"}, Spec: wf.WorkflowSpec{Templates: []wf.Template{template}}, Status: wf.WorkflowStatus{Nodes: wf.Nodes{"n": n}}}
	p := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", Namespace: "ci", UID: "pod", Labels: map[string]string{"workflows.argoproj.io/workflow": "w"}, Annotations: map[string]string{"workflows.argoproj.io/node-id": "n", "workflows.argoproj.io/node-name": "w.build"}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "w", UID: "workflow", Controller: new(true)}}}}
	for _, c := range []struct {
		name string
		code int32
	}{{"main", 0}, {"test", 7}, {"sidecar", 9}, {"wait", 0}} {
		p.Spec.Containers = append(p.Spec.Containers, core.Container{Name: c.name})
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, core.ContainerStatus{Name: c.name, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: c.code}}})
	}
	e := New(Config{Logs: logs})
	defer e.queue.ShutDown()
	tr := &tracking{binding: nativebinding.RunBinding{RunID: "run", Workflow: nativebinding.Object{Name: "w"}}, captures: map[string]bool{}}
	e.tracked[w.Name] = tr
	e.capture(tr, w, []*core.Pod{p})
	entries := tr.pending["build"].entries
	if len(entries) != 4 {
		t.Fatal("missing native containers", entries)
	}
	storeCapturedSetEntries(t, logs, entries)
	body, _, err := logs.Read(t.Context(), "run", runlog.ReadRequest{Selector: runlog.Selector{Step: "build"}, OwnOnly: true})
	if err != nil || string(body) != "main\ntest\n" {
		t.Fatal("container set output was lost or sidecar shown", string(body), err)
	}
	d := diagnose(n, p, false, &template)
	if d.container != "test" || d.exit == nil || *d.exit != 7 {
		t.Fatal("authored failure lost to sidecar or main name", d)
	}
}

func storeCapturedSetEntries(t *testing.T, logs *runlog.Store, entries []runlog.ContainerLog) {
	t.Helper()
	for _, entry := range entries {
		want := entry.Container == "main" || entry.Container == "test"
		if entry.Main != want {
			t.Fatal("wrong authored membership", entry)
		}
		reservation, err := logs.Reserve(t.Context(), "run", entry)
		if err != nil {
			t.Fatal(err)
		}
		if err = reservation.Capture(t.Context(), runlog.CaptureRequest{Source: io.NopCloser(strings.NewReader(entry.Container + "\n"))}); err != nil {
			t.Fatal(err)
		}
	}
}
