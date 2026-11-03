package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const capabilityAnnotation = "oberth.ci/workflow-capability"

var ErrWorkflowCapabilityChanged = errors.New("argojob: workflow capability changed or was revoked")

// WithWorkflowCapabilityLoader supplies the current repository authority. A
// request snapshot alone never authorizes submission or restart recovery.
func (c *Controller) WithWorkflowCapabilityLoader(load func(context.Context, int64) (*argoworkflow.Capability, error)) *Controller {
	c.capabilities = load
	return c
}

func (c *Controller) verifyCapabilityGrant(ctx context.Context, repoID int64, expected *argoworkflow.Capability) error {
	if expected == nil {
		return nil
	}
	if repoID <= 0 || c.capabilities == nil {
		return ErrWorkflowCapabilityChanged
	}
	current, err := c.capabilities(ctx, repoID)
	if err != nil {
		return fmt.Errorf("argojob: read workflow capability: %w", err)
	}
	if current == nil || capabilityBinding(repoID, current) != capabilityBinding(repoID, expected) {
		return ErrWorkflowCapabilityChanged
	}
	return c.verifyCapabilityScope(ctx, current)
}

func (c *Controller) verifyCapabilityScope(ctx context.Context, grant *argoworkflow.Capability) error {
	if err := c.config.validateCapability(grant); err != nil {
		return err
	}
	if c.kube == nil {
		return errors.New("argojob: cannot verify operator ServiceAccount and API scope without a Kubernetes client")
	}
	if _, err := c.kube.CoreV1().ServiceAccounts(grant.Namespace).Get(ctx, grant.ServiceAccount, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("argojob: operator-owned ServiceAccount %s/%s is unavailable: %w", grant.Namespace, grant.ServiceAccount, err)
	}
	for _, kind := range grant.ResourceKinds {
		resources, err := c.kube.Discovery().ServerResourcesForGroupVersion(kind.APIVersion)
		if err != nil {
			return fmt.Errorf("argojob: discover operator resource %s/%s: %w", kind.APIVersion, kind.Kind, err)
		}
		found := false
		if resources != nil {
			for _, r := range resources.APIResources {
				if r.Kind == kind.Kind && !strings.Contains(r.Name, "/") {
					if !r.Namespaced {
						return errors.New("argojob: operator resource capability cannot authorize cluster-scoped objects")
					}
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("argojob: operator resource %s/%s is not discoverable", kind.APIVersion, kind.Kind)
		}
	}
	return nil
}

func (c *Controller) boundCapability(ctx context.Context, wf *wfv1.Workflow, repoID int64) (*argoworkflow.Capability, error) {
	encoded := wf.Annotations[capabilityAnnotation]
	if encoded == "" {
		return nil, nil
	}
	var binding struct {
		RepositoryID int64 `json:"repository_id"`
	}
	if len(encoded) > 256 || json.Unmarshal([]byte(encoded), &binding) != nil || binding.RepositoryID <= 0 || (repoID > 0 && binding.RepositoryID != repoID) || c.capabilities == nil {
		return nil, ErrWorkflowCapabilityChanged
	}
	current, err := c.capabilities(ctx, binding.RepositoryID)
	if err != nil {
		return nil, fmt.Errorf("argojob: read workflow capability: %w", err)
	}
	if current == nil || capabilityBinding(binding.RepositoryID, current) != encoded || current.ServiceAccount != wf.Spec.ServiceAccountName || current.Namespace != wf.Namespace {
		return nil, ErrWorkflowCapabilityChanged
	}
	if err := c.verifyCapabilityScope(ctx, current); err != nil {
		return nil, err
	}
	return current, nil
}

func (config Config) validateCapability(c *argoworkflow.Capability) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	if c.Namespace != config.Namespace {
		return errors.New("argojob: operator capability belongs to a different execution namespace")
	}
	for _, account := range []string{config.PipelineServiceAccount, config.ExecutorServiceAccount, config.VMLauncherServiceAccount, config.CredentialedServiceAccount, config.CISecretsServiceAccount} {
		if c.ServiceAccount == account {
			return errors.New("argojob: operator capability cannot select an Oberth-managed identity")
		}
	}
	for _, accounts := range []map[string]PerRepoIdentityConfig{config.PerRepoIdentities, config.PerRepoCIIdentities, config.PerStepIdentities} {
		for _, account := range accounts {
			if c.ServiceAccount == account.ServiceAccountName {
				return errors.New("argojob: operator capability cannot select a secret-bound identity")
			}
		}
	}
	return nil
}

func capabilityBinding(repoID int64, capability *argoworkflow.Capability) string {
	if capability == nil {
		return ""
	}
	c := *capability
	c.ResourceKinds = slices.Clone(c.ResourceKinds)
	for i := range c.ResourceKinds {
		c.ResourceKinds[i].Actions = slices.Clone(c.ResourceKinds[i].Actions)
		slices.Sort(c.ResourceKinds[i].Actions)
	}
	slices.SortFunc(c.ResourceKinds, func(a, b argoworkflow.ResourceKindCapability) int {
		return strings.Compare(a.APIVersion+"/"+a.Kind, b.APIVersion+"/"+b.Kind)
	})
	body, _ := json.Marshal(c)
	digest := sha256.Sum256(body)
	binding, _ := json.Marshal(struct {
		RepositoryID int64  `json:"repository_id"`
		GrantSHA256  string `json:"grant_sha256"`
	}{repoID, hex.EncodeToString(digest[:])})
	return string(binding)
}
