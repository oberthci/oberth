package argojob_test

import (
	"context"
	"encoding/json"
	"fmt"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	core "k8s.io/api/core/v1"
	"slices"
	"strings"
	"testing"
	"time"
)

func inlineFixtureShape(w *wf.Workflow, kind string) {
	leaf := w.Spec.Templates[0].DeepCopy()
	leaf.Name = ""
	group := wf.Template{Name: "group"}
	if strings.Contains(kind, "dag") {
		group.DAG = &wf.DAGTemplate{Tasks: []wf.DAGTask{{Name: "inline", Inline: leaf}}}
	} else {
		group.Steps = []wf.ParallelSteps{{Steps: []wf.WorkflowStep{{Name: "inline", Inline: leaf}}}}
	}
	if strings.Contains(kind, "pair") {
		first := leaf.DeepCopy()
		first.Container.ReadinessProbe = &core.Probe{ProbeHandler: core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"true"}}}}
		if group.DAG != nil {
			group.DAG.Tasks = []wf.DAGTask{{Name: "first", Inline: first}, {Name: "second", Inline: leaf}}
		} else {
			group.Steps = []wf.ParallelSteps{{Steps: []wf.WorkflowStep{{Name: "first", Inline: first}, {Name: "second", Inline: leaf}}}}
		}
	}
	w.Spec.Entrypoint = "group"
	w.Spec.Templates = []wf.Template{group}
}
func preparedInlineHold(t *testing.T, kind string) (argojob.Config, nativebinding.RunBinding, *[]string) {
	t.Helper()
	p, executor, workflows := fixtureKind(t, kind, nativebinding.Preparation{Assist: true, HelperImage: "oberth.ci/helper:1"})
	view := p.View()
	if len(view.Plan.Entries) != 1 || !view.Plan.Entries[0].HoldArmed || !view.Plan.Entries[0].HoldEligible {
		t.Fatal("inline not eligible", view.Plan)
	}
	b, err := executor.Create(t.Context(), p)
	compositionOK(t, err)
	root := nativeNode(b.Workflow.Name, "", wf.NodeRunning, 1)
	root.Name, root.TemplateName, root.Type = b.Workflow.Name, "group", wf.NodeTypeDAG
	suffix := ".inline"
	if strings.Contains(kind, "steps") {
		suffix = "[0].inline"
		root.Type = wf.NodeTypeSteps
	}
	if view.Plan.Entries[0].Path != strings.TrimPrefix(suffix, ".") {
		t.Fatal("native and admission step paths differ", view.Plan, suffix)
	}
	n := nativeNode("held", "", wf.NodeRunning, 1)
	n.Name, n.DisplayName, n.TemplateName, n.BoundaryID = b.Workflow.Name+suffix, "inline", "", root.ID
	workflows.Object.Status.Nodes = wf.Nodes{root.ID: root, n.ID: n}
	workflows.Object.Status.Phase = wf.WorkflowRunning
	pod := nativePod(workflows.Object, n, 0)
	pod.Status.Phase = core.PodRunning
	pod.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
	pod.Status.ContainerStatuses[0].Ready = true
	calls := []string{}
	pods := &fake.Pods{Object: pod, ExecFunc: func(_ context.Context, q argojob.ExecRequest) error {
		if len(q.Command) < 4 || q.Command[0] != "/oberth-hold/bin/oberth-hold" || q.Command[3] != string(pod.UID) {
			t.Fatal(q)
		}
		calls = append(calls, q.Command[1])
		if q.Command[1] == "status" {
			_, _ = fmt.Fprint(q.Err, "oberth-hold: ")
			return json.NewEncoder(q.Err).Encode(map[string]any{"t": "held", "heldSince": time.Unix(100, 0), "ttl": 1200, "remaining_ms": 60000})
		}
		response := "exit"
		if q.Command[1] == "release" {
			response = "closed"
		}
		_, err := fmt.Fprintf(q.Err, "oberth-hold: {\"t\":%q,\"code\":0}\n", response)
		return err
	}}
	owner := memoryAdmission(t, p)
	return argojob.Config{Runs: owner, Progress: owner, Workflows: workflows, Pods: pods, Cache: &fake.Cache{WorkflowObject: workflows.Object, PodObjects: []*core.Pod{pod}}, Now: compositionClock, Reporter: &reportSpy{}}, b, &calls
}
func TestReviewAdmittedInlineHold(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"review-inline-dag", "review-inline-steps", "review-inline-dag-script", "review-inline-steps-script"} {
		t.Run(kind, func(t *testing.T) {
			c, b, calls := preparedInlineHold(t, kind)
			executor := argojob.New(c)
			rows, err := executor.Holds(t.Context(), b)
			compositionOK(t, err)
			if len(rows) != 1 || rows[0].AvailabilityUnknown {
				t.Fatal("eligible sealed inline hold missing", rows)
			}
			result, err := executor.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: b, Hold: rows[0], Command: []string{"true"}})
			compositionOK(t, err)
			if result.ExitCode != 0 {
				t.Fatal(result)
			}
			compositionOK(t, executor.Abandon(t.Context(), b, rows[0]))
			if !slices.Equal(*calls, []string{"status", "exec", "release"}) {
				t.Fatal("inline action sequence", *calls)
			}
		})
	}
}
func TestReviewInlineHeldLeafDiscovery(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"review-inline-dag", "review-inline-steps", "review-inline-dag-script", "review-inline-steps-script"} {
		t.Run(kind, func(t *testing.T) { checkInlineRefusals(t, kind) })
	}
}
func checkInlineRefusals(t *testing.T, kind string) {
	t.Helper()
	c, b, calls := preparedInlineHold(t, kind)
	executor := argojob.New(c)
	rows, err := executor.Holds(t.Context(), b)
	compositionOK(t, err)
	if len(rows) != 1 {
		t.Fatal("armed inline main invisible", rows)
	}
	h := rows[0]
	w := c.Workflows.(*fake.Workflows).Object
	original := w.Status.Nodes[h.NodeID]
	if original.TemplateName != "" {
		t.Fatal("fixture must use actual inline template name", original)
	}
	for _, field := range []string{"boundary", "call"} {
		n := original
		if field == "boundary" {
			n.BoundaryID = "missing"
		} else {
			n.Name = b.Workflow.Name + ".other"
		}
		w.Status.Nodes[n.ID] = n
		rows, err = executor.Holds(t.Context(), b)
		compositionOK(t, err)
		if len(rows) != 0 {
			t.Fatal("unresolved inline armed", field, rows)
		}
		_, err = executor.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: b, Hold: h, Command: []string{"touch", "effect"}})
		if err == nil || len(*calls) != 1 {
			t.Fatal("unbound inline dispatched", field, err, *calls)
		}
	}
}

func TestReplacedProbeCannotAuthorizeHoldActions(t *testing.T) {
	t.Parallel()
	c, w, p, b := inspectFixture(t)
	executor := argojob.New(c)
	rows, err := executor.Holds(t.Context(), b)
	compositionOK(t, err)
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	h := rows[0]
	w.Object.Spec.Templates[0].Container.ReadinessProbe.Exec.Command = []string{"true"}
	calls := 0
	p.ExecFunc = func(context.Context, argojob.ExecRequest) error { calls++; return nil }
	rows, err = executor.Holds(t.Context(), b)
	compositionOK(t, err)
	if len(rows) != 0 {
		t.Fatal("replaced probe discovered hold", rows)
	}
	_, err = executor.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: b, Hold: h, Command: []string{"touch", "effect"}})
	if err == nil {
		t.Fatal("replaced probe authorized exec")
	}
	if err = executor.Abandon(t.Context(), b, h); err == nil {
		t.Fatal("replaced probe authorized release")
	}
	if calls != 0 {
		t.Fatal("unarmed helper dispatched", calls)
	}
}

func TestInlineSiblingsBindTheirOwnTemplate(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"review-inline-pair-dag", "review-inline-pair-steps"} {
		c, b, calls := pairedInlineFixture(t, kind)
		rows, err := argojob.New(c).Holds(t.Context(), b)
		compositionOK(t, err)
		if len(rows) != 1 || rows[0].NodeID != "second" || len(*calls) != 1 || (*calls)[0] != "pod-second" {
			t.Fatal("inline sibling resolved to another call", rows, *calls)
		}
	}
}
func pairedInlineFixture(t *testing.T, kind string) (argojob.Config, nativebinding.RunBinding, *[]string) {
	t.Helper()
	p, e, workflows := fixtureKind(t, kind, nativebinding.Preparation{Assist: true, HelperImage: "oberth.ci/helper:1"})
	plan := p.View().Plan
	if len(plan.Entries) != 2 || plan.Entries[0].HoldEligible || !plan.Entries[1].HoldEligible {
		t.Fatal(plan)
	}
	b, err := e.Create(t.Context(), p)
	compositionOK(t, err)
	root := nativeNode(b.Workflow.Name, "", wf.NodeRunning, 1)
	root.Name, root.TemplateName, root.Type = b.Workflow.Name, "group", wf.NodeTypeDAG
	prefix := "."
	if strings.Contains(kind, "steps") {
		root.Type = wf.NodeTypeSteps
		prefix = "[0]."
	}
	workflows.Object.Status.Nodes = wf.Nodes{root.ID: root}
	workflows.Object.Status.Phase = wf.WorkflowRunning
	pods := []*core.Pod{}
	for _, name := range []string{"first", "second"} {
		n := nativeNode(name, "", wf.NodeRunning, 1)
		n.Name, n.DisplayName, n.TemplateName, n.BoundaryID = b.Workflow.Name+prefix+name, name, "", root.ID
		workflows.Object.Status.Nodes[n.ID] = n
		pod := nativePod(workflows.Object, n, 0)
		pod.Status.Phase = core.PodRunning
		pod.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
		pod.Status.ContainerStatuses[0].Ready = true
		pods = append(pods, pod)
	}
	calls := []string{}
	client := &fake.Pods{Object: pods[1], ExecFunc: func(_ context.Context, q argojob.ExecRequest) error {
		calls = append(calls, q.Pod)
		_, _ = fmt.Fprint(q.Err, "oberth-hold: ")
		return json.NewEncoder(q.Err).Encode(map[string]any{"t": "held", "heldSince": time.Unix(100, 0), "ttl": 1200, "remaining_ms": 60000})
	}}
	owner := memoryAdmission(t, p)
	return argojob.Config{Runs: owner, Progress: owner, Workflows: workflows, Pods: client, Cache: &fake.Cache{WorkflowObject: workflows.Object, PodObjects: pods}, Now: compositionClock, Reporter: &reportSpy{}}, b, &calls
}
