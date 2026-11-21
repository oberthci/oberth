package argoworkflow

import (
	"context"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/strictjson"
)

// TemplateGetter reads one object in the configured namespace. The caller bounds
// I/O through context; each preparation remembers successes, absence and failures.
type TemplateGetter func(context.Context, string) (*wfv1.WorkflowTemplate, error)

// Policy contains trusted authority, never values decoded from repository YAML.
// PVCClaims and Capability must already be scoped to this repository and trust tier.
type Policy struct {
	// Protected namespaces are required when granting resource authority.
	ServerNamespace string
	ArgoNamespace   string
	// Release identifies a server-verified release run.
	Release            bool
	SecretstoreAddress string
	ImagePrefixes      []string
	PVCClaims          map[string]bool
	Capability         *Capability
	TemplateGetter     TemplateGetter
}

// Capability delegates exact resource envelopes to a distinct executor account.
type Capability struct {
	Namespace      string         `json:"namespace"`
	ServiceAccount string         `json:"service_account"`
	ResourceKinds  []ResourceKind `json:"resource_kinds"`
}
type ResourceKind struct {
	APIVersion string   `json:"api_version"`
	Kind       string   `json:"kind"`
	Actions    []string `json:"actions"`
}

// LoadCapability decodes an operator proposal or committed grant using the
// same shape, protected-namespace and action checks as preparation.
func (p *Policy) LoadCapability(ctx context.Context, namespace string, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var capability Capability
	if strictjson.DecodeStrictObject(raw, &capability, admissioncontract.MaxDocumentBytes) != nil {
		return refusal("policy.capability", "capability")
	}
	proposed := *p
	proposed.Capability = &capability
	g := newGraph(ctx, nil, admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, IdentityRequest: admissioncontract.IdentityRequest{Namespace: namespace}}, proposed)
	g.capabilityPolicy()
	if err := g.err(); err != nil {
		return err
	}
	p.Capability = &capability
	return nil
}
