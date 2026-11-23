package argojob

import (
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLibraryTemplateKeepsAuthoredLogsAndExit(t *testing.T) {
	for _, kind := range []string{"reference", "cluster-reference", "scoped-name", "cluster-scoped-name"} {
		for _, code := range []int32{0, 42} {
			t.Run(kind+strconv.Itoa(int(code)), func(t *testing.T) { checkLibraryOutput(t, kind, code) })
		}
	}
}

func TestInlineLibraryTemplateKeepsAuthoredLogsAndExit(t *testing.T) {
	for _, kind := range []string{"inline-steps", "inline-dag"} {
		for _, code := range []int32{0, 42} {
			t.Run(kind+strconv.Itoa(int(code)), func(t *testing.T) { checkLibraryOutput(t, kind, code) })
		}
	}
}

func checkLibraryOutput(t *testing.T, kind string, code int32) {
	t.Helper()
	w, p := libraryOutputFixture(t, kind, code)
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logs.Close() }()
	e := New(Config{Logs: logs})
	defer e.queue.ShutDown()
	tr := &tracking{binding: nativebinding.RunBinding{RunID: "run", Workflow: nativebinding.Object{Name: w.Name}}, captures: map[string]bool{}}
	e.tracked[w.Name] = tr
	e.capture(t.Context(), tr, w, []*core.Pod{p})
	path := nodePath(w, w.Status.Nodes["n"])
	entries := tr.pending[path].entries
	if len(entries) != 4 {
		t.Fatal("missing native output", entries)
	}
	for _, entry := range entries {
		if entry.Main != (entry.Container == "main" || entry.Container == "lint") {
			t.Fatal("wrong library membership", entry)
		}
		reservation, err := logs.Reserve(t.Context(), "run", entry)
		if err != nil {
			t.Fatal(err)
		}
		if err = reservation.Capture(t.Context(), runlog.CaptureRequest{Source: io.NopCloser(strings.NewReader(entry.Container + "\n"))}); err != nil {
			t.Fatal(err)
		}
	}
	body, _, err := logs.Read(t.Context(), "run", runlog.ReadRequest{Selector: runlog.Selector{Step: path}, OwnOnly: true})
	got := strings.Fields(string(body))
	slices.Sort(got)
	if err != nil || strings.Join(got, " ") != "lint main" {
		t.Fatal("library output lost or executor output shown", string(body), err)
	}
	assertLibraryProjection(t, w, p, code)
}

func libraryOutputFixture(t *testing.T, kind string, code int32) (*wf.Workflow, *core.Pod) {
	t.Helper()
	template := wf.Template{Name: "build", ContainerSet: &wf.ContainerSetTemplate{Containers: []wf.ContainerNode{{Container: core.Container{Name: "main"}}, {Container: core.Container{Name: "lint"}}}}, Sidecars: []wf.UserContainer{{Container: core.Container{Name: "metrics"}}}}
	n := wf.NodeStatus{ID: "n", Name: "w.build", Type: wf.NodeTypePod, Phase: wf.NodeSucceeded, TemplateRef: &wf.TemplateRef{Name: "shared", Template: "build"}}
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "w", Namespace: "ci", UID: "workflow"}, Spec: wf.WorkflowSpec{Templates: []wf.Template{{Name: "build", Container: &core.Container{}}}}, Status: wf.WorkflowStatus{Phase: wf.WorkflowSucceeded}}
	if code != 0 {
		n.Phase, w.Status.Phase = wf.NodeFailed, wf.WorkflowFailed
	}
	if strings.HasPrefix(kind, "cluster-") {
		n.TemplateRef.ClusterScope = true
	}
	if strings.HasSuffix(kind, "scoped-name") {
		scope := wf.ResourceScopeNamespaced
		if n.TemplateRef.ClusterScope {
			scope = wf.ResourceScopeCluster
		}
		n.TemplateRef, n.TemplateName, n.TemplateScope = nil, "build", string(scope)+"/shared"
	}
	scope, resource := n.GetTemplateScope()
	if _, err := w.SetStoredTemplate(scope, resource, &n, &template); err != nil {
		t.Fatal(err)
	}
	w.Status.Nodes = wf.Nodes{}
	if strings.HasPrefix(kind, "inline-") {
		n = inlineLibraryNode(t, w, n, kind)
	}
	w.Status.Nodes[n.ID] = n
	p := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", Namespace: "ci", UID: "pod", Labels: map[string]string{"workflows.argoproj.io/workflow": "w"}, Annotations: map[string]string{"workflows.argoproj.io/node-id": "n", "workflows.argoproj.io/node-name": n.Name}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "w", UID: "workflow", Controller: new(true)}}}}
	for _, c := range []struct {
		name string
		exit int32
	}{{"metrics", 9}, {"main", 0}, {"lint", code}, {"wait", 0}} {
		p.Spec.Containers = append(p.Spec.Containers, core.Container{Name: c.name})
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, core.ContainerStatus{Name: c.name, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: c.exit}}})
	}
	return w, p
}

func inlineLibraryNode(t *testing.T, w *wf.Workflow, n wf.NodeStatus, kind string) wf.NodeStatus {
	t.Helper()
	scope, resource := n.GetTemplateScope()
	inline := w.GetStoredTemplate(scope, resource, &n)
	parent := wf.NodeStatus{ID: "w", Name: "w", Type: wf.NodeTypeDAG, Phase: n.Phase, TemplateRef: &wf.TemplateRef{Name: "shared", Template: "wrapper"}}
	wrapper := wf.Template{Name: "wrapper", DAG: &wf.DAGTemplate{Tasks: []wf.DAGTask{{Name: "build", Inline: inline}}}}
	if kind == "inline-steps" {
		parent.Type, n.Name = wf.NodeTypeSteps, "w[0].build"
		wrapper.DAG = nil
		wrapper.Steps = []wf.ParallelSteps{{Steps: []wf.WorkflowStep{{Name: "build", Inline: inline}}}}
	}
	if _, err := w.SetStoredTemplate(wf.ResourceScopeLocal, "", &parent, &wrapper); err != nil {
		t.Fatal(err)
	}
	w.Status.Nodes[parent.ID] = parent
	n.TemplateRef, n.TemplateName, n.TemplateScope, n.BoundaryID = nil, "", "namespaced/shared", parent.ID
	return n
}

func assertLibraryProjection(t *testing.T, w *wf.Workflow, p *core.Pod, code int32) {
	t.Helper()
	out, err := Project(t.Context(), Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: nodePath(w, w.Status.Nodes["n"])}}}, Workflow: w, Pods: []*core.Pod{p}})
	if err != nil || len(out.Nodes) != 1 || len(out.Steps) != 1 || out.Nodes[0].Exit == nil || *out.Nodes[0].Exit != int(code) {
		t.Fatal("library exit evidence lost", out, err)
	}
	if code == 0 {
		if out.Steps[0].Status != stepmodel.StepPassed {
			t.Fatal("library success lost", out)
		}
		return
	}
	if out.Steps[0].DisplayReason != "exited with code 42" || out.Steps[0].Logs[0].Container != "lint" || out.Failure == nil || out.Failure.Class != stepmodel.FailureAuthor {
		t.Fatal("library failure lost", out)
	}
}
