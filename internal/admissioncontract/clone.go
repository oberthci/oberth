package admissioncontract

import (
	"maps"
	"slices"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

func (p Plan) Clone() Plan {
	p.Entries = slices.Clone(p.Entries)
	return p
}

func (s TemplateSnapshot) Clone() TemplateSnapshot {
	s.Spec = slices.Clone(s.Spec)
	return s
}

func (e IdentityEvidence) Clone() IdentityEvidence {
	if e.Resource != nil {
		copy := *e.Resource
		e.Resource = &copy
	}
	return e
}

func (v PreparedView) Clone() PreparedView {
	v.Workflow = v.Workflow.DeepCopy()
	v.SourceBindings = cloneBindings(v.SourceBindings)
	v.Defaults = v.Defaults.Clone()
	v.Templates = slices.Clone(v.Templates)
	for i := range v.Templates {
		v.Templates[i] = v.Templates[i].Clone()
	}
	v.Identity = v.Identity.Clone()
	v.Grants = slices.Clone(v.Grants)
	v.Plan = v.Plan.Clone()
	v.Findings = slices.Clone(v.Findings)
	return v
}

func (r PrepareRequest) Clone() PrepareRequest {
	r.Document = slices.Clone(r.Document)
	r.Defaults = r.Defaults.Clone()
	r.Templates = slices.Clone(r.Templates)
	for i := range r.Templates {
		r.Templates[i] = r.Templates[i].Clone()
	}
	r.Grants = slices.Clone(r.Grants)
	return r
}

func (i TransformInput) Clone() TransformInput {
	i.Plan = i.Plan.Clone()
	i.CredentialSteps = maps.Clone(i.CredentialSteps)
	i.Identity = i.Identity.Clone()
	i.SecretGrants = maps.Clone(i.SecretGrants)
	for key, grants := range i.SecretGrants {
		i.SecretGrants[key] = slices.Clone(grants)
	}
	i.SourceBindings = cloneBindings(i.SourceBindings)
	i.Defaults = i.Defaults.Clone()
	return i
}

func cloneBindings(bindings []sourcecontract.Binding) []sourcecontract.Binding {
	copy := slices.Clone(bindings)
	for i := range copy {
		copy[i] = copy[i].Clone()
	}
	return copy
}
