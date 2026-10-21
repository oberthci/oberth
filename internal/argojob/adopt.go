package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// Adoption pins observation to the exact Workflow found during owner recovery.
// Resource size comes from that submission, never from the current default branch.
type Adoption struct {
	Size       periapsis.Size
	UID        types.UID
	Identity   string
	SpecSHA256 string
}

type AdoptionRequest struct {
	Name, RunID, Ref, SHA, Group string
	Trigger                      periapsis.Trigger
	RunTrigger                   argoworkflow.RunTrigger
}

func (controller *Controller) Adopt(ctx context.Context, request AdoptionRequest) (Adoption, bool, error) {
	workflow, err := controller.workflows.Get(ctx, request.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Adoption{}, false, nil
	}
	if err != nil {
		return Adoption{}, false, fmt.Errorf("argojob: read Workflow for adoption: %w", err)
	}
	if err := controller.verifyOwnership(workflow, request.RunID); err != nil {
		return Adoption{}, false, err
	}
	_, identity, _ := WorkflowMeta(workflow)
	digest, digestErr := hex.DecodeString(identity)
	if request.RunID == "" || workflow.Name != request.Name || workflow.Namespace != controller.config.Namespace ||
		workflow.UID == "" || workflow.DeletionTimestamp != nil || digestErr != nil || len(digest) != 32 ||
		workflow.Annotations[refAnnotation] != request.Ref || workflow.Labels["oberth.ci/sha"] != labelValue(request.SHA) ||
		workflow.Labels["oberth.ci/trigger"] != string(request.Trigger) {
		return Adoption{}, false, fmt.Errorf("argojob: Workflow %s adoption binding mismatch", request.Name)
	}
	group, err := argoworkflow.RunConcurrencyGroup(workflow)
	if err != nil || group != request.Group {
		return Adoption{}, false, fmt.Errorf("argojob: Workflow %s adoption group mismatch", request.Name)
	}
	size, err := argoworkflow.DeclaredSizeFor(workflow, request.RunTrigger)
	if err != nil {
		return Adoption{}, false, err
	}
	specHash, err := adoptionSpecHash(workflow)
	if err != nil {
		return Adoption{}, false, err
	}
	return Adoption{Size: size, UID: workflow.UID, Identity: identity, SpecSHA256: specHash}, true, nil
}

// Argo's status and API metadata change while it runs. Its live spec must stay
// fixed, even if a replacement keeps the original identity annotation.
func adoptionSpecHash(workflow *wfv1.Workflow) (string, error) {
	body, err := json.Marshal(workflow.Spec)
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
