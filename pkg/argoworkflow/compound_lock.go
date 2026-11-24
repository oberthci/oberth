package argoworkflow

import (
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

func (g *graph) templateLocks(t *wfv1.Template, path string) {
	g.synchronization(t.Synchronization, path)
	if len(t.Steps) > 0 || t.DAG != nil {
		g.compoundLock(t.Synchronization, path, false)
	}
}

func (g *graph) compoundLock(s *wfv1.Synchronization, path string, workflow bool) {
	if s == nil || len(s.Mutexes)+len(s.Semaphores) == 0 {
		return
	}
	reason := "compound lock holds while children wait"
	if workflow {
		reason = "workflow lock spans the whole run"
	}
	g.findings = append(g.findings, admissioncontract.Finding{
		Advisory: true, Path: path, Code: "compound_lock", Reason: reason,
		Remedy: "Move this lock to the leaf templates that use the protected resource.",
	})
}
