package argojob

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// WithWorkflowCapabilityRecoveryLoader binds observation/token recovery to the
// store's exact pre-migration in-flight submission receipt. Create and its
// retries keep verifyCapabilityGrant, which never consults this loader.
func (c *Controller) WithWorkflowCapabilityRecoveryLoader(load func(context.Context, int64, string, string, string) (json.RawMessage, error)) *Controller {
	c.capabilityRecovery = load
	return c
}

func recoveredCapabilityBinding(repoID int64, raw json.RawMessage, current *argoworkflow.Capability) string {
	// This historical wire shape is read only from the authenticated migration
	// receipt. It is not a grant input and cannot restore device ceilings.
	var previous struct {
		Namespace         string                                `json:"namespace"`
		ServiceAccount    string                                `json:"service_account"`
		ResourceKinds     []argoworkflow.ResourceKindCapability `json:"resource_kinds,omitempty"`
		ExtendedResources map[string]int64                      `json:"extended_resources,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&previous) != nil || len(previous.ExtendedResources) == 0 {
		return ""
	}
	projection := &argoworkflow.Capability{Namespace: previous.Namespace, ServiceAccount: previous.ServiceAccount, ResourceKinds: previous.ResourceKinds}
	if projection.Validate() != nil || capabilityBinding(repoID, projection) != capabilityBinding(repoID, current) {
		return ""
	}
	previous.ResourceKinds = orderedCapability(projection).ResourceKinds
	return encodeCapabilityBinding(repoID, previous)
}
