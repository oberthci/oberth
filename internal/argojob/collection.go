package argojob

import (
	"context"
	"encoding/json"
	"slices"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const workflowRetentionSeconds int32 = 86400
const collectionHoldLabel = "oberth.ci/archive-held"

// Watch replay resumes collection after a crash between Finish and the patch.
func (e *Executor) resumeCollection(ctx context.Context, name string) bool {
	if e.config.Cache == nil || e.config.Runs == nil {
		return false
	}
	w, ok := e.config.Cache.Workflow(name)
	if !ok || !collectionCandidate(w) {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, e.config.ObservationTimeout)
	defer cancel()
	r, err := e.config.Runs.Run(ctx, w.Labels[nativebinding.RunLabel])
	if err != nil && publicerr.Classify(err).Code() == publicerr.NotFound {
		return false
	}
	if err == nil && !terminalChild(r) {
		return false
	}
	if err == nil {
		err = e.restoreCollection(ctx, r, w)
	}
	if err != nil {
		e.logError(ctx, name, err)
		e.queue.AddRateLimited(name)
	} else {
		e.queue.Forget(name)
	}
	return true
}

func collectionCandidate(w *wf.Workflow) bool {
	return terminalWorkflow(w) && w.Labels[nativebinding.EngineLabel] == nativebinding.EngineValue && (w.Spec.TTLStrategy == nil || slices.Contains(w.Finalizers, nativebinding.RecordFinalizer))
}

func (e *Executor) restoreCollection(ctx context.Context, r stateport.Run, w *wf.Workflow) error {
	var child *childObservation
	var err error
	if scheduledChildRun(r) {
		child, err = e.loadChild(ctx, r.ID)
		if err != nil {
			return err
		}
	}
	if err = e.restoreSnapshot(ctx, r, child); err != nil {
		return err
	}
	e.mu.Lock()
	e.tracked[r.Origin.WorkflowName].historical = w.Spec.TTLStrategy == nil && !slices.Contains(w.Finalizers, nativebinding.RecordFinalizer)
	e.mu.Unlock()
	return nil
}

// A live identity check fences collection to the bound Workflow. Historical
// Workflows without a TTL get the same one-day debug retention after Finish.
func (e *Executor) allowCollection(ctx context.Context, b nativebinding.RunBinding, historical bool) error {
	if err := e.collectionRead(ctx); err != nil {
		return err
	}
	w, err := e.config.Workflows.Get(ctx, b.Workflow.Name, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = e.verifyObserved(ctx, b, w); err != nil {
		return err
	}
	if !collectionAllowed(w, historical) {
		return nil
	}
	if err := e.requireArchives(ctx, b, w); err != nil {
		return err
	}
	return e.releaseCollected(ctx, b, w)
}

func collectionAllowed(w *wf.Workflow, historical bool) bool {
	return terminalWorkflow(w) && (slices.Contains(w.Finalizers, nativebinding.RecordFinalizer) || historical && w.Spec.TTLStrategy == nil)
}

func (e *Executor) releaseCollected(ctx context.Context, b nativebinding.RunBinding, w *wf.Workflow) error {
	finalizers := slices.DeleteFunc(slices.Clone(w.Finalizers), func(f string) bool { return f == nativebinding.RecordFinalizer })
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": b.Workflow.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": w.ResourceVersion},
		{"op": "add", "path": "/metadata/finalizers", "value": finalizers},
	}
	if w.Spec.TTLStrategy == nil {
		patch = append(patch, map[string]any{"op": "add", "path": "/spec/ttlStrategy", "value": &wf.TTLStrategy{SecondsAfterCompletion: new(workflowRetentionSeconds)}})
	}
	body, _ := json.Marshal(patch)
	_, err := e.config.Workflows.Patch(ctx, PatchRequest{Name: w.Name, Body: body})
	return err
}

func prepareCollection(w *wf.Workflow) {
	if w.Spec.TTLStrategy == nil {
		w.Spec.TTLStrategy = &wf.TTLStrategy{SecondsAfterCompletion: new(workflowRetentionSeconds)}
	}
	if w.Spec.PodGC != nil {
		// Collection belongs to Workflow TTL after archive, including when the
		// author requested earlier Pod GC. Completed Workflows are not reconciled
		// again by Argo, so no post-completion policy restoration is attempted.
		w.Spec.PodGC = &wf.PodGC{Strategy: wf.PodGCOnWorkflowCompletion, LabelSelector: &meta.LabelSelector{MatchExpressions: []meta.LabelSelectorRequirement{
			{Key: collectionHoldLabel, Operator: meta.LabelSelectorOpExists},
			{Key: collectionHoldLabel, Operator: meta.LabelSelectorOpDoesNotExist},
		}}}
	}
}
