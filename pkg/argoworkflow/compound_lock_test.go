package argoworkflow

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	corev1 "k8s.io/api/core/v1"
)

func TestCompoundLocksAreAdvisoriesInInspectAndSubmit(t *testing.T) {
	for _, kind := range []string{"mutex", "semaphore"} {
		for _, carrier := range []string{"workflow", "steps", "dag", "leaf"} {
			for _, mode := range []admissioncontract.Mode{admissioncontract.ModeInspect, admissioncontract.ModeSubmit} {
				t.Run(kind+"/"+carrier+"/"+string(mode), func(t *testing.T) {
					checkCompoundLock(t, kind, carrier, mode)
				})
			}
		}
	}
}

func checkCompoundLock(t *testing.T, kind, carrier string, mode admissioncontract.Mode) {
	t.Helper()
	w, wantPath := compoundLockWorkflow(kind, carrier)
	r := requestFor(t, w)
	r.Mode = mode
	r.IdentityRequest.Mode = mode
	before := bytes.Clone(r.Document)
	prepared, err := Prepare(t.Context(), r, Policy{}, identityHooks())
	if err != nil || prepared == nil {
		t.Fatal("lock placement refused admission", err)
	}
	if !bytes.Equal(before, r.Document) || !reflect.DeepEqual(w.Spec.Synchronization, prepared.View().Workflow.Spec.Synchronization) {
		t.Fatal("admission rewrote authored locks")
	}
	assertCompoundLockFinding(t, prepared.View().Findings, wantPath, carrier)
}

func compoundLockWorkflow(kind, carrier string) (*wfv1.Workflow, string) {
	w := simpleWorkflow()
	sync := &wfv1.Synchronization{Mutexes: []*wfv1.Mutex{{Name: "forge/org/repo/publish"}}}
	if kind == "semaphore" {
		sync = &wfv1.Synchronization{Semaphores: []*wfv1.SemaphoreRef{{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "oberth-sync-limits"}, Key: "build"}}}}
	}
	wantPath := `spec.templates["parent"].synchronization`
	switch carrier {
	case "workflow":
		w.Spec.Synchronization, wantPath = sync, "spec.synchronization"
	case "leaf":
		w.Spec.Templates[0].Synchronization = sync
	case "steps", "dag":
		parent := wfv1.Template{Name: "parent", Synchronization: sync}
		if carrier == "steps" {
			parent.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "child", Template: "main"}}}}
		} else {
			parent.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "child", Template: "main"}}}
		}
		w.Spec.Entrypoint = parent.Name
		w.Spec.Templates = append(w.Spec.Templates, parent)
	}
	return w, wantPath
}

func assertCompoundLockFinding(t *testing.T, findings []admissioncontract.Finding, wantPath, carrier string) {
	t.Helper()
	if carrier == "leaf" {
		if len(findings) != 0 {
			t.Fatal("leaf lock reported", findings)
		}
		return
	}
	if len(findings) != 1 || !findings[0].Advisory || findings[0].Path != wantPath || findings[0].Code != "compound_lock" || !strings.Contains(findings[0].Remedy, "leaf") {
		t.Fatal("missing non-blocking lock finding", findings)
	}
	if len(findings[0].Reason) > 40 || len(findings[0].Remedy) > 120 {
		t.Fatal("finding exceeds message bounds")
	}
}
