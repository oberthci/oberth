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

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Adoption pins observation to the exact Workflow found during owner recovery.
// Source and execution identity come from that submission.
type Adoption struct {
	Sources     string
	RestartedAt time.Time
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
	if err := validateHelperStorage(workflow); err != nil {
		return Adoption{}, err
	}
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
	if workflow.Annotations[capabilityAnnotation] == "" {
		for _, template := range workflow.Spec.Templates {
			if template.Resource != nil {
				return Adoption{}, argoworkflow.ErrWorkflowCapabilityRequired
			}
		}
	}
	if !request.RunTrigger.Valid() {
		return Adoption{}, fmt.Errorf("invalid run trigger %q", request.RunTrigger)
	}

	specHash, err := adoptionSpecHash(workflow)
	if err != nil {
		return Adoption{}, err
	}
	return Adoption{Sources: workflow.Annotations[argoworkflow.SourcesAnnotation], RestartedAt: controller.restartedAt, UID: workflow.UID, Identity: identity, SpecSHA256: specHash}, nil
}

// Pinned Argo's SuspendWorkflow/ResumeWorkflow change only spec.suspend;
// StopWorkflow/TerminateWorkflow change spec.shutdown (workflow/util/util.go).
// Their other changes are status and API metadata. All execution fields remain
// bound, even if a replacement keeps the original identity annotation.
func adoptionSpecHash(workflow *wfv1.Workflow) (string, error) {
	spec := workflow.Spec
	switch spec.Shutdown {
	case "", wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate:
	default:
		return "", errors.New("argojob: invalid native Workflow shutdown strategy")
	}
	spec.Suspend = nil
	spec.Shutdown = ""

	if binding := workflow.Annotations[capabilityAnnotation]; binding != "" {
		body, err := json.Marshal(struct {
			Spec       wfv1.WorkflowSpec
			Capability string
		}{spec, binding})
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256(body)
		return hex.EncodeToString(digest[:]), nil
	}
	body, err := json.Marshal(struct {
		Spec wfv1.WorkflowSpec `json:"spec"`
	}{spec})
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
