package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Adoption pins observation to the exact Workflow found during owner recovery.
// Resource size comes from that submission, never from the current default branch.
type Adoption struct {
	Sources     string
	Fallback    model.ResourceAdmissionFallback
	Pods        argoworkflow.PodDemand
	RestartedAt time.Time
	Size        pipeline.Size
	UID         types.UID
	Identity    string
	SpecSHA256  string
}

// ErrAdoptionRefused marks a Workflow that exists but must not be observed as
// the run's: it stays refused until the operator deletes it. A failed read is
// not a refusal.
var ErrAdoptionRefused = errors.New("argojob: Workflow refused for adoption")

type AdoptionRequest struct {
	PinnedSources         []pipeline.PinnedSource
	RepositoryID          int64
	Name, RunID, Ref, SHA string
	Trigger               pipeline.Trigger
	RunTrigger            argoworkflow.RunTrigger
	ScheduleEntry         string
}

func (controller *Controller) Adopt(ctx context.Context, request AdoptionRequest) (Adoption, bool, error) {
	workflow, err := controller.workflows.Get(ctx, request.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Adoption{}, false, nil
	}
	if err != nil {
		return Adoption{}, false, fmt.Errorf("argojob: read Workflow for adoption: %w", err)
	}
	if workflow.Annotations[capabilityAnnotation] != "" {
		if request.RepositoryID <= 0 {
			return Adoption{}, false, fmt.Errorf("%w: missing capability repository binding", ErrAdoptionRefused)
		}
		if _, err := controller.boundCapability(ctx, workflow, request.RepositoryID); err != nil {
			return Adoption{}, false, fmt.Errorf("%w: %w", ErrAdoptionRefused, err)
		}
	}
	binding, err := controller.bindAdoption(workflow, request)
	if err != nil {
		return Adoption{}, false, fmt.Errorf("%w: %w", ErrAdoptionRefused, err)
	}
	return binding, true, nil
}

func (controller *Controller) bindAdoption(workflow *wfv1.Workflow, request AdoptionRequest) (Adoption, error) {
	if err := verifyDeliveredPinnedSources(workflow, request.PinnedSources); err != nil {
		return Adoption{}, err
	}
	if err := controller.verifyOwnership(workflow, request.RunID); err != nil {
		return Adoption{}, err
	}
	_, identity, _ := WorkflowMeta(workflow)
	digest, digestErr := hex.DecodeString(identity)
	if request.RunID == "" || workflow.Name != request.Name || workflow.Namespace != controller.config.Namespace ||
		workflow.UID == "" || workflow.DeletionTimestamp != nil || digestErr != nil || len(digest) != 32 ||
		workflow.Annotations[refAnnotation] != request.Ref || workflow.Labels["oberth.ci/sha"] != labelValue(request.SHA) ||
		workflow.Labels[triggerLabel] != string(request.Trigger) {
		return Adoption{}, fmt.Errorf("argojob: Workflow %s adoption binding mismatch", request.Name)
	}
	if !argoworkflow.ScheduleEntryMatches(workflow, request.RunTrigger, request.ScheduleEntry) {
		return Adoption{}, fmt.Errorf("argojob: Workflow %s adoption schedule entry mismatch", request.Name)
	}
	if err := argoworkflow.RefuseRetiredWorkflow(workflow); err != nil {
		return Adoption{}, err
	}
	if workflow.Annotations[capabilityAnnotation] == "" {
		for _, template := range workflow.Spec.Templates {
			if template.Resource != nil {
				return Adoption{}, argoworkflow.ErrWorkflowCapabilityRequired
			}
		}
	}
	size, err := argoworkflow.DeclaredSizeFor(workflow, request.RunTrigger)
	if err != nil {
		return Adoption{}, err
	}

	specHash, err := adoptionSpecHash(workflow)
	if err != nil {
		return Adoption{}, err
	}
	pods, err := readPodDemand(workflow)
	if err != nil {
		// Admission metadata is advisory. Keep the immutable spec binding and
		// recover scheduling credits from owned live Pods instead.
		log.Printf("Workflow %s: CPU demand binding unreadable; using live Pod requests: %v", workflow.Name, err)
		pods = nil
	}
	var peak argoworkflow.PodDemand
	if pods != nil {
		peak = *pods
	}
	fallback, err := readResourceFallback(workflow)
	if err != nil {
		return Adoption{}, err
	}
	return Adoption{Sources: workflow.Annotations[argoworkflow.SourcesAnnotation], Fallback: fallback, Pods: peak, RestartedAt: controller.restartedAt, Size: size, UID: workflow.UID, Identity: identity, SpecSHA256: specHash}, nil
}

// Argo's status and API metadata change while it runs. Its live spec must stay
// fixed, even if a replacement keeps the original identity annotation.
func adoptionSpecHash(workflow *wfv1.Workflow) (string, error) {
	if binding := workflow.Annotations[capabilityAnnotation]; binding != "" {
		body, err := json.Marshal(struct {
			Spec                            wfv1.WorkflowSpec
			PodDemand, Fallback, Capability string
			CPURequests                     string `json:"cpu_requests,omitempty"`
		}{workflow.Spec, workflow.Annotations[podDemandAnnotation], workflow.Annotations[resourceFallbackAnnotation], binding, workflow.Annotations[cpuRequestsAnnotation]})
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256(body)
		return hex.EncodeToString(digest[:]), nil
	}
	body, err := json.Marshal(struct {
		Spec        wfv1.WorkflowSpec `json:"spec"`
		PodDemand   string            `json:"pod_demand,omitempty"`
		Fallback    string            `json:"resource_fallback,omitempty"`
		CPURequests string            `json:"cpu_requests,omitempty"`
	}{workflow.Spec, workflow.Annotations[podDemandAnnotation], workflow.Annotations[resourceFallbackAnnotation], workflow.Annotations[cpuRequestsAnnotation]})
	if err != nil {
		return "", fmt.Errorf("argojob: encode adopted Workflow spec: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func (binding Adoption) matches(workflow *wfv1.Workflow) bool {
	if workflow.Annotations[argoworkflow.SourcesAnnotation] != binding.Sources || workflow.UID != binding.UID || workflow.Annotations[identityAnnotation] != binding.Identity {
		return false
	}
	digest, err := adoptionSpecHash(workflow)
	return err == nil && digest == binding.SpecSHA256
}
