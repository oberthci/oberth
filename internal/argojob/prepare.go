package argojob

import (
	"context"
	"fmt"
	"github.com/oberthci/oberth/internal/nativebinding"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
)

// Hooks serve one admission; its steps share each account's single read.
func (e *Executor) Hooks(p nativebinding.Preparation) admissioncontract.PrepareHooks {
	a := admission{Executor: e, accounts: map[string]admissioncontract.AccountEvidence{}}
	return admissioncontract.PrepareHooks{LoginAudience: e.config.SecretStore.Access.Audience, VerifyIdentities: a.verifyIdentities, TransformNative: func(ctx context.Context, w *wf.Workflow, in admissioncontract.TransformInput) error {
		return a.transform(ctx, w, in, p)
	}}
}

// admission is one preparation's view of the cluster. Its hooks run one after
// another, so accounts needs no lock; it is dropped with the hooks.
type admission struct {
	*Executor
	accounts map[string]admissioncontract.AccountEvidence
}

func (e admission) verifyIdentities(ctx context.Context, r admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
	out := admissioncontract.IdentityEvidence{Namespace: r.Namespace, State: admissioncontract.StateVerified}
	names := []string{r.PipelineAccount}
	if r.ResourceAccount != "" {
		names = append(names, r.ResourceAccount)
	}
	for i, name := range names {
		account, err := e.verifyAccount(ctx, name)
		if err != nil {
			if r.Mode != admissioncontract.ModeInspect || !apierrors.IsNotFound(err) {
				return out, err
			}
			account = admissioncontract.AccountEvidence{Name: name}
			out.State = admissioncontract.StateLiveRequired
		}
		if i == 0 {
			out.Pipeline = account
		} else {
			out.Resource = &account
		}
	}
	return out, nil
}
func (e admission) verifyAccount(ctx context.Context, name string) (admissioncontract.AccountEvidence, error) {
	if evidence, read := e.accounts[name]; read {
		return evidence, nil
	}
	a, err := e.config.Accounts.Get(ctx, name, meta.GetOptions{})
	if err != nil {
		return admissioncontract.AccountEvidence{}, accountMissing(false, fmt.Errorf("account %s: %w", name, err))
	}
	if a.Name != name || a.UID == "" {
		return admissioncontract.AccountEvidence{}, accountMissing(false, fmt.Errorf("account %s has no matching UID", name))
	}
	evidence := admissioncontract.AccountEvidence{Name: a.Name, UID: string(a.UID)}
	e.accounts[name] = evidence
	return evidence, nil
}
func (e admission) transform(ctx context.Context, w *wf.Workflow, in admissioncontract.TransformInput, p nativebinding.Preparation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if admissioncontract.ScheduleParent(in.Mode, model.RunTrigger(p.Trigger)) {
		v := admissioncontract.PreparedView{Ref: p.Ref.Name, CommitSHA: p.Ref.CommitSHA, Trigger: model.TriggerSchedule}
		stampSubmission(w, v, p.RepositoryID)
		stampChildIdentity(w)
	}
	w.Spec.Executor, w.Spec.PodSpecPatch = &wf.ExecutorConfig{ServiceAccountName: in.Identity.Pipeline.Name}, executorTokenPatch
	return eachTemplate(w, func(t *wf.Template, key string) error { return e.transformLeaf(ctx, nativeLeaf{w, t, key}, in, p) })
}

type nativeLeaf struct {
	workflow *wf.Workflow
	template *wf.Template
	key      string
}

func (e admission) transformLeaf(ctx context.Context, leaf nativeLeaf, in admissioncontract.TransformInput, p nativebinding.Preparation) error {
	t, key, w := leaf.template, leaf.key, leaf.workflow
	if err := ctx.Err(); err != nil {
		return err
	}
	if !t.IsLeaf() {
		return nil
	}
	grants := in.SecretGrants[key]
	account, err := leafAccount(leaf, in, p, grants)
	if err != nil {
		return err
	}
	if in.Mode == admissioncontract.ModeSubmit {
		if _, err := e.verifyAccount(ctx, account); err != nil {
			return accountMissing(!p.Release, fmt.Errorf("step %s: %w", t.Name, err))
		}
	}
	t.ServiceAccountName, t.Executor = account, &wf.ExecutorConfig{ServiceAccountName: in.Identity.Pipeline.Name}
	appendSourceTrust(t, in.SourceBindings)
	if err := e.appendSecrets(t, grants); err != nil {
		return err
	}
	if err := appendSecretHelper(leaf, p.HelperImage, len(grants) > 0); err != nil {
		return err
	}
	entry, ok := planTemplate(in.Plan, key)
	if !ok {
		return nil
	}
	if err := armHold(t, w, p, entry); err != nil {
		return err
	}
	recordHoldArming(in.Plan, key, p.Assist && entry.HoldEligible)
	return nil
}

func leafAccount(leaf nativeLeaf, in admissioncontract.TransformInput, p nativebinding.Preparation, grants []admissioncontract.GrantEvidence) (string, error) {
	t := leaf.template
	if t.Resource != nil && in.Identity.Resource != nil {
		return in.Identity.Resource.Name, nil
	}
	key := identityname.Key{Repository: p.Repository}
	kind := identityname.Pipeline
	if !p.Release {
		kind = identityname.PipelineCI
	}
	if step := in.CredentialSteps[leaf.key]; p.Release && step != "" {
		key.File, key.Step, kind = p.File, step, identityname.Step
	}
	if !p.Release && len(grants) > 0 {
		key, kind = grants[0].Key(), identityname.StepCI
	}

	return identityname.Name(key, kind)
}

const executorTokenPatch = `{"volumes":[{"name":"exec-sa-token","projected":{"sources":[{"serviceAccountToken":{"expirationSeconds":3600,"path":"token"}},{"configMap":{"name":"kube-root-ca.crt","items":[{"key":"ca.crt","path":"ca.crt"}]}},{"downwardAPI":{"items":[{"path":"namespace","fieldRef":{"apiVersion":"v1","fieldPath":"metadata.namespace"}}]}}]},"secret":null}]}`

func eachTemplate(w *wf.Workflow, visit func(*wf.Template, string) error) error {
	for i := range w.Spec.Templates {
		t := &w.Spec.Templates[i]
		if err := visitTemplate(templateVisit{template: t, key: t.Name, path: fmt.Sprintf("spec.templates[%q]", t.Name)}, visit); err != nil {
			return err
		}
	}
	return nil
}

type templateVisit struct {
	template  *wf.Template
	key, path string
}

func visitTemplate(v templateVisit, visit func(*wf.Template, string) error) error {
	if err := visit(v.template, v.key); err != nil {
		return err
	}
	for _, child := range inlineTemplates(v) {
		if err := visitTemplate(child, visit); err != nil {
			return err
		}
	}
	return nil
}
func inlineTemplates(v templateVisit) []templateVisit {
	out := []templateVisit{}
	for group, steps := range v.template.Steps {
		for _, step := range steps.Steps {
			if step.Inline != nil {
				key := fmt.Sprintf("%s.steps[%d][%q]", v.path, group, step.Name)
				out = append(out, templateVisit{step.Inline, key, key + ".inline"})
			}
		}
	}
	if v.template.DAG != nil {
		for _, task := range v.template.DAG.Tasks {
			if task.Inline != nil {
				key := v.path + ".dag.tasks[" + task.Name + "]"
				out = append(out, templateVisit{task.Inline, key, key + ".inline"})
			}
		}
	}
	return out
}
func planTemplate(plan admissioncontract.Plan, key string) (admissioncontract.PlanEntry, bool) {
	found := admissioncontract.PlanEntry{HoldEligible: true}
	known := false
	for _, entry := range plan.Entries {
		if entry.Template == key {
			known = true
			found.HoldEligible = found.HoldEligible && entry.HoldEligible
			if found.HoldRefusal == "" {
				found.HoldRefusal = entry.HoldRefusal
			}
			found.Template = key
		}
	}
	if !known {
		found.HoldEligible = false
	}
	return found, known
}

func recordHoldArming(plan admissioncontract.Plan, key string, armed bool) {
	for i := range plan.Entries {
		if plan.Entries[i].Template == key {
			plan.Entries[i].HoldArmed = armed
		}
	}
}
