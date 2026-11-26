package argojob

import (
	"context"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
)

// The immutable proof and native metadata accompany tracking, so observations
// retain no second spec or plan and need no durable reads after restoration.
type childObservation struct {
	proof     nativebinding.ChildBinding
	shutdown  wf.ShutdownStrategy
	admission string
	// refused is the terminal fact of a child whose source could not be kept.
	refused *Observation
}

func boundChild(r stateport.Run, proof nativebinding.ChildBinding) (*childObservation, error) {
	b := runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: r.Admission.Identity.Namespace, Name: r.Origin.WorkflowName, UID: r.Origin.NativeUID()})
	if err := b.Verify(proof.Run); err != nil {
		return nil, ownership(err)
	}
	if r.Admission.Workflow == nil {
		return nil, ownership(nil)
	}
	w := r.Admission.Workflow
	return &childObservation{proof: proof, shutdown: w.Spec.Shutdown, admission: w.Annotations[nativebinding.AdmissionAnnotation]}, nil
}

func (c *childObservation) verify(children Children, w *wf.Workflow) error {
	if err := children.VerifyChild(w, c.proof); err != nil {
		return ownership(err)
	}
	return c.verifySpec(w)
}

func (e *Executor) verifyObserved(ctx context.Context, b nativebinding.RunBinding, w *wf.Workflow) error {
	if b.Trigger != string(model.TriggerSchedule) || b.Workflow.Name != b.RunID {
		return verifyWorkflow(b, w)
	}
	if e.config.ChildRuns == nil || e.config.Children == nil || w == nil {
		return ownership(nil)
	}
	e.mu.Lock()
	t := e.tracked[b.Workflow.Name]
	var child *childObservation
	if t != nil && t.binding == b {
		child = t.child
	}
	e.mu.Unlock()
	if child == nil {
		var err error
		child, err = e.loadChild(ctx, b.RunID)
		if err != nil {
			return err
		}
	}
	if err := b.Verify(child.proof.Run); err != nil {
		return ownership(err)
	}
	return child.verify(e.config.Children, w)
}

func (e *Executor) loadChild(ctx context.Context, id string) (*childObservation, error) {
	proof, err := e.config.ChildRuns.ChildProof(ctx, id)
	if err != nil {
		return nil, err
	}
	r, err := e.config.Runs.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	return boundChild(r, proof)
}

func (c *childObservation) verifySpec(w *wf.Workflow) error {
	digest, err := admissioncontract.ChildSpecDigest(w.Spec, c.shutdown)
	if err != nil || digest != c.proof.Run.SpecSHA256 || w.Annotations[nativebinding.AdmissionAnnotation] != c.admission {
		return ownership(err)
	}
	return nil
}
