package argoworkflow

import "github.com/oberthci/oberth/internal/admissioncontract"

const (
	holdRetriedGroup = "inside a retried group"
	holdLoop         = "inside a loop"
	holdSharedUse    = "its template is also used where assist cannot apply"
)

type holdInvocations struct {
	uses          map[*node]map[string]bool
	omitted       map[*call]bool
	workflowRetry bool
}

// holdInvocationRefusals includes uses hidden by bounded or dynamic plan entries.
// Each resolved node/context is visited once, independently of invocation count.
func (g *graph) holdInvocationRefusals(plan admissioncontract.Plan, omitted map[*call]bool) map[string]string {
	u := holdInvocations{uses: map[*node]map[string]bool{}, omitted: omitted, workflowRetry: g.workflow.Spec.RetryStrategy != nil}
	for _, c := range g.roots {
		u.invoke(c, "")
	}
	for _, e := range plan.Entries {
		if e.Dynamic {
			u.visit(g.byKey[e.Template], holdSharedUse)
		}
	}
	refusals := map[string]string{}
	for n, reasons := range u.uses {
		refusals[n.key] = holdUseRefusal(reasons)
	}
	return refusals
}
func (u *holdInvocations) invoke(c *call, reason string) {
	if c.inspectionOnly {
		return
	}
	if c.dynamic || c.items != nil {
		reason = holdLoop
	} else if u.omitted[c] && reason == "" {
		reason = holdSharedUse
	}
	if c.target != nil {
		u.visit(c.target, reason)
	}
	for _, n := range c.candidates {
		u.visit(n, reason)
	}
	for _, h := range c.hooks {
		u.invoke(h, reason)
	}
}
func (u *holdInvocations) visit(n *node, reason string) {
	if n == nil {
		return
	}
	reasons := u.uses[n]
	if reasons[reason] {
		return
	}
	if reasons == nil {
		reasons = map[string]bool{}
		u.uses[n] = reasons
	}
	reasons[reason] = true
	if reason == "" && !leaf(n.template) && (n.template.RetryStrategy != nil || u.workflowRetry) {
		reason = holdRetriedGroup
	}
	for _, c := range n.calls {
		u.invoke(c, reason)
	}
}

func holdUseRefusal(reasons map[string]bool) string {
	if reasons[""] && len(reasons) > 1 {
		return holdSharedUse
	}
	for _, reason := range []string{holdLoop, holdRetriedGroup, holdSharedUse} {
		if reasons[reason] {
			return reason
		}
	}
	return ""
}
