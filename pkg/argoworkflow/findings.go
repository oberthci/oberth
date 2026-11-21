package argoworkflow

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func validGrantRequest(key workflowpolicy.GrantKey) bool {
	if key.Repository == "" {
		key.Repository = "forge/team/repo"
	}
	return key.Validate() == nil
}

func grantableRequest(key workflowpolicy.GrantKey, kind, value string) bool {
	return validGrantRequest(key) && (kind == "secret" || kind == "identity" || kind == deviceGrant || publicClaim(value))
}

func (g *graph) commandFile() string {
	if g.offlineValidation && g.unknownRole() {
		return "<file>"
	}
	return g.request.SelectedFile
}

func (g *graph) collect(err error) bool {
	var admission *AdmissionError
	if !errors.As(err, &admission) {
		return false
	}
	g.findings = append(g.findings, admission.Findings...)
	g.findings = append(g.findings, admission.Needs...)
	if admission.Detail != "" {
		g.details = append(g.details, admission.Detail)
	}
	return true
}

func (g *graph) sortFindings(findings []admissioncontract.Finding) {
	rank := func(path string) int {
		for i, n := range g.nodes {
			if path == n.path || strings.HasPrefix(path, n.path+".") {
				return i + 1
			}
		}
		return 0
	}
	slices.SortStableFunc(findings, func(a, b admissioncontract.Finding) int {
		if order := cmp.Compare(rank(a.Path), rank(b.Path)); order != 0 {
			return order
		}
		if order := strings.Compare(a.Path, b.Path); order != 0 {
			return order
		}
		return strings.Compare(a.Code, b.Code)
	})
}

func (g *graph) missingCapability(path, namespace string, kind ResourceKind) {
	if slices.Contains(g.protectedNamespaces(), namespace) {
		g.refuse(path, "protected_namespace")
		return
	}
	f := finding(path, "capability")
	f.Installation = true
	repo := g.request.Repository
	if repo == "" {
		repo = "<repo>"
	}
	capability := Capability{Namespace: namespace, ServiceAccount: "<resource-account>"}
	if g.policy.Capability != nil {
		capability.ServiceAccount = g.policy.Capability.ServiceAccount
		if g.policy.Capability.Namespace == namespace {
			capability.ResourceKinds = slices.Clone(g.policy.Capability.ResourceKinds)
		}
	}
	g.requiredResources(&capability, kind)
	raw, _ := json.Marshal(capability)
	text := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(string(raw))
	f.Command = publicerr.CapabilityGrantCommand(repo, text)
	g.findings = append(g.findings, f)
}

func (g *graph) requiredResources(c *Capability, required ResourceKind) {
	mergeResource(c, required)
	for _, n := range g.nodes {
		if n.template.Resource == nil || !g.resourceShape(n) {
			continue
		}
		obj, ok := resourceEnvelope(n.template.Resource, g.request.IdentityRequest.Namespace)
		metadata, _ := obj["metadata"].(map[string]any)
		kind := ResourceKind{APIVersion: stringField(obj, "apiVersion"), Kind: stringField(obj, "kind"), Actions: []string{n.template.Resource.Action}}
		if ok && stringField(metadata, "namespace") == c.Namespace && validResourceKind(kind) {
			mergeResource(c, kind)
		}
	}
}

func mergeResource(c *Capability, kind ResourceKind) {
	for i, existing := range c.ResourceKinds {
		if existing.APIVersion != kind.APIVersion || existing.Kind != kind.Kind {
			continue
		}
		actions := append(slices.Clone(existing.Actions), kind.Actions...)
		slices.Sort(actions)
		c.ResourceKinds[i].Actions = slices.Compact(actions)
		return
	}
	c.ResourceKinds = append(c.ResourceKinds, kind)
}

// Authored refusals stop live hooks, but do not hide independent native faults.
func (g *graph) diagnose(w *wf.Workflow) error {
	if g.stopped || len(g.reads) != 0 {
		return g.err()
	}
	copy := w.DeepCopy()
	g.stampParameters(copy, g.request)
	if g.policy.Release {
		// Facts are read only for an admitted release; the copy never runs.
		copy = releaseSchemaParameters(copy)
	}
	final := g.finalGraph(copy)
	if err := final.nativeSchema(copy, admissioncontract.IdentityEvidence{}); err != nil {
		var native *AdmissionError
		if !errors.As(err, &native) {
			return err
		}
		for _, f := range native.Findings {
			covered := slices.ContainsFunc(g.findings, func(prior admissioncontract.Finding) bool {
				return !prior.Installation && (f.Path == prior.Path || strings.HasPrefix(f.Path, prior.Path+"."))
			})
			if !covered {
				g.findings = append(g.findings, f)
			}
		}
		g.details = append(g.details, native.Detail)
	}
	return g.err()
}
