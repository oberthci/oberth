package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// Adoption pins observation to the exact Workflow found during owner recovery.
// Resource size comes from that submission, never from the current default branch.
type Adoption struct {
	Fallback    model.ResourceAdmissionFallback
	Pods        argoworkflow.PodDemand
	RestartedAt time.Time

	Size       periapsis.Size
	UID        types.UID
	Identity   string
	SpecSHA256 string
}

// ErrAdoptionRefused marks a Workflow that exists but must not be observed as
// the run's: it stays refused until the operator deletes it. A failed read is
// not a refusal.
var ErrAdoptionRefused = errors.New("argojob: Workflow refused for adoption")

type AdoptionRequest struct {
	RepositoryID                 int64
	Name, RunID, Ref, SHA, Group string
	Trigger                      periapsis.Trigger
	RunTrigger                   argoworkflow.RunTrigger
	ScheduleEntry                string
}

func (controller *Controller) Adopt(ctx context.Context, request AdoptionRequest) (Adoption, bool, error) {
	workflow, err := controller.workflows.Get(ctx, request.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Adoption{}, false, nil
	}
	if err != nil {
		return Adoption{}, false, fmt.Errorf("argojob: read Workflow for adoption: %w", err)
	}
	resources, extended := argoworkflow.CapabilityUsage(workflow)
	if (resources || extended) && workflow.Annotations[capabilityAnnotation] == "" {
		return Adoption{}, false, fmt.Errorf("%w: resource Workflow lacks a current operator capability binding; drain legacy resource workflows before upgrading", ErrAdoptionRefused)
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
	if err := controller.verifyOwnership(workflow, request.RunID); err != nil {
		return Adoption{}, err
	}
	_, identity, _ := WorkflowMeta(workflow)
	digest, digestErr := hex.DecodeString(identity)
	if request.RunID == "" || workflow.Name != request.Name || workflow.Namespace != controller.config.Namespace ||
		workflow.UID == "" || workflow.DeletionTimestamp != nil || digestErr != nil || len(digest) != 32 ||
		workflow.Annotations[refAnnotation] != request.Ref || workflow.Labels["oberth.ci/sha"] != labelValue(request.SHA) ||
		workflow.Labels["oberth.ci/trigger"] != string(request.Trigger) {
		return Adoption{}, fmt.Errorf("argojob: Workflow %s adoption binding mismatch", request.Name)
	}
	if !argoworkflow.ScheduleEntryMatches(workflow, request.RunTrigger, request.ScheduleEntry) {
		return Adoption{}, fmt.Errorf("argojob: Workflow %s adoption schedule entry mismatch", request.Name)
	}
	group, err := argoworkflow.DeclaredConcurrencyGroup(workflow)
	if err != nil || group != request.Group {
		return Adoption{}, fmt.Errorf("argojob: Workflow %s adoption group mismatch", request.Name)
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
		return Adoption{}, err
	}
	var peak argoworkflow.PodDemand
	if pods != nil {
		peak = *pods
	}
	fallback, err := readResourceFallback(workflow)
	if err != nil {
		return Adoption{}, err
	}
	return Adoption{Fallback: fallback, Pods: peak, RestartedAt: controller.restartedAt, Size: size, UID: workflow.UID, Identity: identity, SpecSHA256: specHash}, nil
}

// Argo's status and API metadata change while it runs. Its live spec must stay
// fixed, even if a replacement keeps the original identity annotation.
func adoptionSpecHash(workflow *wfv1.Workflow) (string, error) {
	body, err := json.Marshal(struct {
		Spec       wfv1.WorkflowSpec `json:"spec"`
		PodDemand  string            `json:"pod_demand,omitempty"`
		Fallback   string            `json:"resource_fallback,omitempty"`
		Capability string            `json:"capability,omitempty"`
	}{workflow.Spec, workflow.Annotations[podDemandAnnotation], workflow.Annotations[resourceFallbackAnnotation], workflow.Annotations[capabilityAnnotation]})
	if err != nil {
		return "", fmt.Errorf("argojob: encode adopted Workflow spec: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func (binding Adoption) matches(workflow *wfv1.Workflow) bool {
	if workflow.UID != binding.UID || workflow.Annotations[identityAnnotation] != binding.Identity {
		return false
	}
	digest, err := adoptionSpecHash(workflow)
	return err == nil && digest == binding.SpecSHA256
}
