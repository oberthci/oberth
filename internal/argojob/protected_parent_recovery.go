package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (c *Controller) placedParent(name, runID string) bool {
	p := c.config.ExecutionPlacement
	return p.Protected() && name == p.document.NormalWorkflow && runID == p.document.ParentRunID
}

func (c *Controller) preparePlacedParentWorkflow(ctx context.Context, wf *wfv1.Workflow) error {
	if !c.placedParent(wf.Name, wf.Annotations[runIDAnnotation]) {
		return nil
	}
	if c.config.ExecutionPlacement.observationOnly || c.placementJournal == nil {
		return errors.New("argojob: parent intent lacks allocation authority")
	}
	if err := c.checkPlacedAllocation(ctx, wf.Name); err != nil {
		return err
	}
	body, err := json.Marshal(wf)
	if err != nil {
		return err
	}
	return c.placementJournal.PreparePlacementWorkflow(ctx, wf.Annotations[runIDAnnotation], wf.Name, body)
}

func (c *Controller) bindPlacedParentWorkflow(ctx context.Context, wf *wfv1.Workflow) error {
	return c.checkPlacedParentWorkflow(ctx, wf, true)
}

func (c *Controller) checkPlacedParentWorkflow(ctx context.Context, wf *wfv1.Workflow, bind bool) error {
	p := c.config.ExecutionPlacement
	if !p.Protected() || wf.Name != p.document.NormalWorkflow {
		return nil
	}
	if wf.Namespace != c.config.Namespace || wf.UID == "" || len(wf.OwnerReferences) != 0 || c.placementJournal == nil {
		return errors.New("argojob: placed parent identity unavailable")
	}
	if err := p.verifyEnvironment(ctx, c.kube, c.config); err != nil {
		return err
	}
	body, uid, err := c.placementJournal.PlacementWorkflow(ctx, p.document.ParentRunID, wf.Name)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("argojob: placed parent has no pre-POST intent")
	}
	if !bind && uid == "" {
		return errors.New("argojob: parent Workflow UID has not been observed")
	}
	if uid != "" && uid != string(wf.UID) {
		return errors.New("argojob: replacement parent Workflow UID")
	}
	var intended wfv1.Workflow
	if json.Unmarshal(body, &intended) != nil || intended.Name != wf.Name || intended.Namespace != wf.Namespace {
		return errors.New("argojob: corrupt parent Workflow custody")
	}
	if err = sameSubmission(wf, &intended); err != nil {
		return err
	}
	for name, value := range intended.Annotations {
		if wf.Annotations[name] != value {
			return errors.New("argojob: parent annotations differ from pre-POST intent")
		}
	}
	if !apiequality.Semantic.DeepEqual(wf.Spec, intended.Spec) {
		return errors.New("argojob: parent spec differs from pre-POST intent")
	}
	if !bind {
		return nil
	}
	return c.placementJournal.BindPlacementWorkflowUID(ctx, p.document.ParentRunID, wf.Name, string(wf.UID))
}

func (c *Controller) settlePlacedParent(ctx context.Context, name, runID string, notBefore time.Time) (bool, error) {
	budget, cancel := context.WithTimeout(ctx, workflowCreateCleanupAttemptBudget)
	defer cancel()
	if err := c.config.ExecutionPlacement.verifyEnvironment(budget, c.kube, c.config); err != nil {
		return false, err
	}
	wf, err := c.workflows.Get(budget, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	absent := apierrors.IsNotFound(err)
	if !absent {
		if err = c.bindPlacedParentWorkflow(budget, wf); err != nil {
			return false, err
		}
	}
	_, uid, err := c.placementJournal.PlacementWorkflow(budget, runID, name)
	if err != nil {
		return false, err
	}
	// An unknown/late POST may still commit. Neither early NotFound nor deleting
	// an observed object can discharge that original observation boundary.
	if absent && time.Now().Before(notBefore) {
		return false, nil
	}
	cleaned, err := c.cleanOwnedWorkflowOnce(budget, name, runID, types.UID(uid), false)
	return cleaned && !time.Now().Before(notBefore), err
}

func (c *Controller) cancelPlacedParent(ctx context.Context, name, runID string) error {
	// Cancellation waits for original Workflow, Pods and source absence. The
	// scheduler's separate uncertain-create path supplies its late-POST bound.
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		clean, err := c.settlePlacedParent(ctx, name, runID, time.Now())
		if err != nil {
			return err
		}
		if clean {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
