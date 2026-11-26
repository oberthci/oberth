package argojob_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argoutil "github.com/argoproj/argo-workflows/v4/workflow/util"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
)

const resumeDAG = `{"apiVersion":"argoproj.io/v1alpha1","kind":"Workflow","spec":{"entrypoint":"main","templates":[
{"name":"main","dag":{"tasks":[{"name":"a","template":"step"},{"name":"b","template":"step","depends":"a"},{"name":"c","template":"step","depends":"b"}]}},
{"name":"step","container":{"image":"example.test/build:1","command":["sh","-c","test -e FIXED"]}}]}}`

func resumeNow() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }

// resumeFixture is R1 after Argo failed its step b: a passed, c never ran,
// collection released the record finalizer and every Pod ended.
func resumeFixture(t *testing.T, document string) argojob.ResumeInput {
	t.Helper()
	r := requestForProgress()
	p := compositionDocument(t, admissionInput{id: "run-0000000000000001", request: r, document: []byte(document)})
	s := memoryAdmissionFor(t, p, r)
	f := &fake.Workflows{}
	b, err := argojob.New(argojob.Config{Workflows: f, Runs: s, Progress: s, Now: compositionClock, Cache: &fake.Cache{}, Reporter: &reportSpy{}}).Create(t.Context(), p)
	compositionOK(t, err)
	w := f.Object.DeepCopy()
	w.Finalizers = slices.DeleteFunc(w.Finalizers, func(f string) bool { return f == nativebinding.RecordFinalizer })
	w.ResourceVersion = "7"
	finished := meta.NewTime(resumeNow().Add(-time.Hour))
	w.Status = wf.WorkflowStatus{Phase: wf.WorkflowFailed, StartedAt: meta.NewTime(resumeNow().Add(-2 * time.Hour)), FinishedAt: finished}
	root := wf.NodeStatus{ID: w.Name, Name: w.Name, DisplayName: w.Name, TemplateName: "main", Type: wf.NodeTypeDAG, Phase: wf.NodeFailed, Children: []string{w.Name + "-1"}, FinishedAt: finished}
	a := resumeNode(w, "1", "a", wf.NodeSucceeded)
	a.Children = []string{w.Name + "-2"}
	failed := resumeNode(w, "2", "b", wf.NodeFailed)
	root.OutboundNodes = []string{failed.ID}
	w.Status.Nodes = wf.Nodes{root.ID: root, a.ID: a, failed.ID: failed}
	pods := []*core.Pod{resumePod(w, a, core.PodSucceeded), resumePod(w, failed, core.PodFailed)}
	return argojob.ResumeInput{Predecessor: b, Sealed: p.View(), Workflow: w, Pods: pods, RunID: "run-0000000000000002", Commit: strings.Repeat("f", 40), Now: resumeNow()}
}

func resumeNode(w *wf.Workflow, id, task string, phase wf.NodePhase) wf.NodeStatus {
	return wf.NodeStatus{ID: w.Name + "-" + id, Name: w.Name + "." + task, DisplayName: task, TemplateName: "step", Type: wf.NodeTypePod, Phase: phase, BoundaryID: w.Name, StartedAt: meta.NewTime(resumeNow().Add(-90 * time.Minute)), FinishedAt: meta.NewTime(resumeNow().Add(-time.Hour))}
}

func resumePod(w *wf.Workflow, n wf.NodeStatus, phase core.PodPhase) *core.Pod {
	controller := true
	name := argoutil.GeneratePodName(w.Name, n.Name, n.TemplateName, n.ID, argoutil.GetWorkflowPodNameVersion(w))
	return &core.Pod{ObjectMeta: meta.ObjectMeta{Name: name, UID: types.UID("pod-" + n.ID), Labels: map[string]string{"workflows.argoproj.io/workflow": w.Name}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: w.Name, UID: w.UID, Controller: &controller}}}, Status: core.PodStatus{Phase: phase}}
}

// The failed node and everything after it run again on the fixed commit;
// the passed node is carried, and only the failed Pod is deleted.
func TestResumeCarriesPassedAndRunsFixedCommit(t *testing.T) {
	t.Parallel()
	in := resumeFixture(t, resumeDAG)
	before := in.Workflow.DeepCopy()
	plan, err := argojob.PlanResume(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Carried, []string{"a"}) || !slices.Equal(plan.Remaining, []string{"b", "c"}) {
		t.Fatal("remaining set", plan.Carried, plan.Remaining)
	}
	if len(plan.Pods) != 1 || plan.Pods[0].Name != in.Pods[1].Name || plan.Pods[0].UID != in.Pods[1].UID {
		t.Fatal("deleted Pods", plan.Pods)
	}
	checkResumeWrite(t, in, before, plan)
	if !jsonEqual(t, in.Workflow, before) {
		t.Fatal("planning mutated the live object")
	}
}

// checkResumeWrite: routing labels name the successor, identity stays R1's,
// and the native retry dropped the failed node and kept the passed one.
func checkResumeWrite(t *testing.T, in argojob.ResumeInput, before *wf.Workflow, plan argojob.ResumePlan) {
	t.Helper()
	w := plan.Workflow
	want := map[string]string{nativebinding.RunLabel: in.RunID, nativebinding.CommitLabel: in.Commit}
	for key, value := range map[string]string{nativebinding.RunIDAnnotation: in.RunID, nativebinding.RefAnnotation: before.Annotations[nativebinding.RefAnnotation], nativebinding.IdentityAnnotation: before.Annotations[nativebinding.IdentityAnnotation]} {
		if w.Annotations[key] != value {
			t.Fatal("annotation", key, w.Annotations)
		}
	}
	for key, value := range want {
		if w.Labels[key] != value {
			t.Fatal("routing label not moved", key, w.Labels)
		}
	}
	if w.UID != before.UID || w.Name != before.Name || plan.UID != before.UID {
		t.Fatal("identity", w.ObjectMeta)
	}
	if plan.ResourceVersion != "7" || !slices.Contains(w.Finalizers, nativebinding.RecordFinalizer) {
		t.Fatal("resource version or finalizer", plan.ResourceVersion, w.Finalizers)
	}
	checkRetriedNodes(t, w, before.Name)
}

func checkRetriedNodes(t *testing.T, w *wf.Workflow, name string) {
	t.Helper()
	if _, ok := w.Status.Nodes[name+"-2"]; ok || w.Status.Nodes[name+"-1"].Phase != wf.NodeSucceeded || w.Status.Phase != wf.WorkflowUnknown {
		t.Fatal("native retry result", w.Status)
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	compositionOK(t, err)
	y, err := json.Marshal(b)
	compositionOK(t, err)
	return string(x) == string(y)
}

func refusalReason(t *testing.T, err error) string {
	t.Helper()
	var p *publicerr.Error
	if !errors.As(err, &p) {
		t.Fatalf("not a public refusal: %v", err)
	}
	return p.Reason()
}

// Each precondition refuses alone, before any effect; the unchanged fixture
// resumes, which is the removal control for every row.
func TestResumeRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		change     func(*argojob.ResumeInput)
	}{
		{"short TTL", argojob.ReasonResumeWindow, func(in *argojob.ResumeInput) {
			in.Workflow.Status.FinishedAt = meta.NewTime(resumeNow().Add(-23*time.Hour - time.Minute))
		}},
		{"deleting", argojob.ReasonResumeStart, func(in *argojob.ResumeInput) { in.Workflow.DeletionTimestamp = &meta.Time{Time: resumeNow()} }},
		{"running", argojob.ReasonResumeStart, func(in *argojob.ResumeInput) { in.Workflow.Status.Phase = wf.WorkflowRunning }},
		{"stopped", argojob.ReasonResumeStart, func(in *argojob.ResumeInput) { in.Workflow.Spec.Shutdown = wf.ShutdownStrategyStop }},
		{"offloaded", argojob.ReasonResumeStart, func(in *argojob.ResumeInput) { in.Workflow.Status.OffloadNodeStatusVersion = "v1" }},
		{"finalizer held", "run is still finishing", func(in *argojob.ResumeInput) {
			in.Workflow.Finalizers = append(in.Workflow.Finalizers, nativebinding.RecordFinalizer)
		}},
		{"live Pod", "run is still finishing", func(in *argojob.ResumeInput) { in.Pods[0].Status.Phase = core.PodRunning }},
		{"spec changed", argojob.ReasonResumeChanged, func(in *argojob.ResumeInput) {
			in.Workflow.Spec.Templates[1].Container.Image = "example.test/other:1"
		}},
		{"workspace deleted", argojob.ReasonResumeChanged, func(in *argojob.ResumeInput) {
			for _, w := range []*wf.Workflow{in.Workflow, in.Sealed.Workflow} {
				w.Spec.VolumeClaimTemplates = []core.PersistentVolumeClaim{{ObjectMeta: meta.ObjectMeta{Name: "work"}}}
				w.Spec.VolumeClaimGC = &wf.VolumeClaimGC{Strategy: wf.VolumeClaimGCOnCompletion}
			}
		}},
		{"unsafe remaining step", argojob.ReasonResumeUnsafe, func(in *argojob.ResumeInput) {
			for i := range in.Sealed.Plan.Entries {
				if in.Sealed.Plan.Entries[i].Path == "c" {
					in.Sealed.Plan.Entries[i].OutputParameters = true
				}
			}
		}},
		{"foreign Pod", "Workflow ownership differs", func(in *argojob.ResumeInput) { in.Pods[1].OwnerReferences[0].UID = "other" }},
		{"foreign Workflow", "Workflow ownership differs", func(in *argojob.ResumeInput) { in.Workflow.Labels[nativebinding.RunLabel] = "run-other" }},
		{"undecodable nodes", argojob.ReasonResumeStart, func(in *argojob.ResumeInput) {
			in.Workflow.Status.Nodes, in.Workflow.Status.CompressedNodes = nil, "not compressed"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := resumeFixture(t, resumeDAG)
			tc.change(&in)
			_, err := argojob.PlanResume(t.Context(), in)
			if got := refusalReason(t, err); got != tc.want {
				t.Fatalf("got %q, want %q (%v)", got, tc.want, err)
			}
		})
	}
	if _, err := argojob.PlanResume(t.Context(), resumeFixture(t, resumeDAG)); err != nil {
		t.Fatal("control refused", err)
	}
}

func TestResumeAllowsSecretStepAfterFailure(t *testing.T) {
	t.Parallel()
	in := resumeFixture(t, resumeDAG)
	for i := range in.Sealed.Plan.Entries {
		in.Sealed.Plan.Entries[i].SecretRoute = true
	}
	plan, err := argojob.PlanResume(t.Context(), in)
	compositionOK(t, err)
	if len(plan.SecretSteps) != 2 {
		t.Fatal("secret steps missing from intent", plan.SecretSteps)
	}
}

// A failed node with a succeeded descendant stays as Argo leaves it, so the
// failed entry still counts as remaining (classified as Argo classifies it).
func TestResumeFailedNodeWithSucceededDescendant(t *testing.T) {
	t.Parallel()
	in := resumeFixture(t, resumeDAG)
	w := in.Workflow
	failed := w.Status.Nodes[w.Name+"-2"]
	failed.Children = []string{w.Name + "-3"}
	w.Status.Nodes[failed.ID] = failed
	after := resumeNode(w, "3", "c", wf.NodeSucceeded)
	w.Status.Nodes[after.ID] = after
	in.Pods = append(in.Pods, resumePod(w, after, core.PodSucceeded))
	plan, err := argojob.PlanResume(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := plan.Workflow.Status.Nodes[failed.ID]; !kept || !slices.Contains(plan.Remaining, "b") || len(plan.Pods) != 0 {
		t.Fatal("Argo classification differs", plan.Remaining, plan.Pods)
	}
}

// A Pod already gone counts as deleted: Argo's names stay the set, so a crash
// after the deletes plans the same write again.
func TestResumePodSetIsArgosNames(t *testing.T) {
	t.Parallel()
	in := resumeFixture(t, resumeDAG)
	first, err := argojob.PlanResume(t.Context(), in)
	compositionOK(t, err)
	in.Pods = in.Pods[:1]
	again, err := argojob.PlanResume(t.Context(), in)
	compositionOK(t, err)
	if len(again.Pods) != 1 || again.Pods[0].Name != first.Pods[0].Name || again.Pods[0].UID != "" {
		t.Fatal("pod set after deletes", again.Pods)
	}
}

// The write is size-checked before any Pod is named for deletion.
func TestResumeSizeCheckedFirst(t *testing.T) {
	in := resumeFixture(t, resumeDAG)
	t.Setenv("MAX_WORKFLOW_SIZE", "2000")
	_, err := argojob.PlanResume(t.Context(), in)
	if got := refusalReason(t, err); got != "Workflow too large to resume" {
		t.Fatal(got, err)
	}
}
