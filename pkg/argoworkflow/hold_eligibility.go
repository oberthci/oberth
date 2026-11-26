package argoworkflow

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

// eligibleHoldPlan makes each authored decision once, before native injections.
func (g *graph) eligibleHoldPlan() admissioncontract.Plan {
	omitted := map[*call]bool{}
	plan := g.planWithOmissions(omitted)
	uses := g.holdInvocationRefusals(plan, omitted)
	resultConsumed := g.consumesResult()
	for i := range plan.Entries {
		e := &plan.Entries[i]
		e.ResultConsumed = resultConsumed
		if n := g.byKey[e.Template]; n != nil {
			e.SecretRoute = e.CredentialIntent || holdReadsSecret(n.template, g.workflow.Spec.Volumes) || n.template.Resource != nil || n.template.ServiceAccountName != ""
			e.OutputParameters = len(n.template.Outputs.Parameters) > 0
		}
		e.HoldRefusal = uses[e.Template]
		if e.HoldRefusal == "" {
			e.HoldRefusal = g.holdRefusal(*e)
		}
		e.HoldEligible = e.HoldRefusal == ""
	}
	return plan
}

var resumeResultReference = regexp.MustCompile(`(?:outputs|\[\s*['"]outputs['"]\s*\])\s*(?:\.\s*result|\[\s*['"]result['"]\s*\])`)

// Include external and inline effective templates. Conservatively refuse every
// held producer when the graph consumes result; its stdout is append-only.
func (g *graph) consumesResult() bool {
	values := []any{g.workflow.Spec}
	for _, n := range g.nodes {
		values = append(values, n.template)
	}
	for _, v := range values {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		// Unescape JSON strings before recognizing expression indexing syntax.
		if resumeResultReference.MatchString(strings.ReplaceAll(string(b), `\"`, `"`)) {
			return true
		}
	}
	return false
}
func (g *graph) holdRefusal(e admissioncontract.PlanEntry) string {
	n := g.byKey[e.Template]
	if n == nil {
		return "eligibility unresolved"
	}
	t := n.template
	if e.CredentialIntent || holdReadsSecret(t, g.workflow.Spec.Volumes) {
		return "reads a secret"
	}
	if holdNamespaceCollision(t) {
		return "uses helper namespace"
	}
	if g.holdUsesArtifacts(n) {
		return "uses artifacts"
	}
	if reason := holdShapeRefusal(t); reason != "" {
		return reason
	}
	r := t.RetryStrategy
	if r == nil {
		r = g.workflow.Spec.RetryStrategy
	}
	return holdRetryRefusal(r)
}
func holdShapeRefusal(t *wf.Template) string {
	var main *core.Container
	if t.Container != nil {
		main = t.Container
	} else if t.Script != nil {
		main = &t.Script.Container
	}
	if main == nil {
		return "not a container or script"
	}
	if len(main.Command) == 0 || main.Command[0] == "" || strings.Contains(main.Command[0], "{{") {
		return "no explicit command"
	}
	if t.Daemon != nil && *t.Daemon {
		return "is a daemon"
	}
	return holdProbeRefusal(main)
}
func holdProbeRefusal(main *core.Container) string {
	if main.ReadinessProbe != nil || main.StartupProbe != nil || main.LivenessProbe != nil {
		return "has authored probes"
	}
	if main.RestartPolicy != nil || len(main.RestartPolicyRules) > 0 {
		return "has container restart policy"
	}
	return ""
}
func holdRetryRefusal(r *wf.RetryStrategy) string {
	if r == nil {
		return ""
	}
	if r.Expression != "" || r.Limit == nil {
		return "has unsupported retries"
	}
	policy := r.RetryPolicyActual()
	if policy != wf.RetryPolicyOnFailure && policy != wf.RetryPolicyAlways {
		return "has unsupported retry policy"
	}
	limit, err := strconv.ParseInt(r.Limit.String(), 10, 64)
	if err != nil || limit < 0 {
		return "has unresolved retry limit"
	}
	return ""
}
func holdReadsSecret(t *wf.Template, shared []core.Volume) bool {
	mounted := map[string]bool{}
	for _, c := range containers(t) {
		if secretEnvironment(c.container) {
			return true
		}
		for _, m := range c.container.VolumeMounts {
			mounted[m.Name] = true
		}
	}
	for _, v := range append(slices.Clone(shared), t.Volumes...) {
		if mounted[v.Name] && holdSecretVolume(v) {
			return true
		}
	}
	return false
}
func secretEnvironment(c *core.Container) bool {
	return slices.ContainsFunc(c.Env, func(e core.EnvVar) bool { return e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil }) || slices.ContainsFunc(c.EnvFrom, func(e core.EnvFromSource) bool { return e.SecretRef != nil })
}
func holdSecretVolume(v core.Volume) bool {
	if v.Secret != nil || v.CSI != nil {
		return true
	}
	return v.Projected != nil && slices.ContainsFunc(v.Projected.Sources, func(p core.VolumeProjection) bool { return p.Secret != nil || p.ServiceAccountToken != nil })
}
func (g *graph) holdUsesArtifacts(n *node) bool {
	if n.template.ArchiveLocation != nil || len(n.template.Outputs.Artifacts) > 0 {
		return true
	}
	for _, a := range n.template.Inputs.Artifacts {
		origins := g.inventory.all[slot{n, a.Name}]
		if len(origins) == 0 {
			return true
		}
		for source := range origins {
			if !source.Kind.Valid() {
				return true
			}
		}
	}
	return false
}
func frozenHoldPlan(observed, authored admissioncontract.Plan) admissioncontract.Plan {
	entries := make(map[string]admissioncontract.PlanEntry, len(authored.Entries))
	for _, e := range authored.Entries {
		entries[e.Path] = e
	}
	for i := range observed.Entries {
		frozen := entries[observed.Entries[i].Path]
		observed.Entries[i].SecretRoute, observed.Entries[i].OutputParameters = frozen.SecretRoute, frozen.OutputParameters
		observed.Entries[i].ResultConsumed = frozen.ResultConsumed
		observed.Entries[i].HoldArmed = frozen.HoldArmed
		observed.Entries[i].HoldEligible, observed.Entries[i].HoldRefusal = frozen.HoldEligible, frozen.HoldRefusal
	}
	return observed
}

func holdNamespaceCollision(t *wf.Template) bool {
	for _, c := range containers(t) {
		for _, e := range c.container.Env {
			if strings.HasPrefix(e.Name, "OBERTH_HOLD_") {
				return true
			}
		}
		for _, m := range c.container.VolumeMounts {
			if overlaps(m.MountPath, "/oberth-hold") {
				return true
			}
		}
		for _, d := range c.container.VolumeDevices {
			if overlaps(d.DevicePath, "/oberth-hold") {
				return true
			}
		}
	}
	return false
}
