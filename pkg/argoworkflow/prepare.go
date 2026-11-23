package argoworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow/defaults"
	"github.com/oberthci/oberth/pkg/argoworkflow/specsize"
)

// Prepare is the sole producer of a sealed admitted result. Audit, Create and
// explanation consume copies of this result; no consumer recompiles the YAML.
func Prepare(ctx context.Context, request admissioncontract.PrepareRequest, policy Policy, hooks admissioncontract.PrepareHooks) (prepared *admissioncontract.PreparedAdmission, resultErr error) {
	defer func() { resultErr = fileError(request.SelectedFile, resultErr) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := request.Clone()
	if err := releaseSelection(r, policy); err != nil {
		return nil, err
	}
	wf, err := prepareDocument(r)
	if err != nil {
		return nil, err
	}
	r.IdentityRequest.Mode = r.Mode
	g := policy.admitRequest(ctx, wf, r, hooks)
	if err := g.err(); err != nil {
		return nil, g.diagnose(wf)
	}
	identity, err := verifyIdentity(ctx, r, hooks)
	if err != nil {
		return nil, err
	}
	if r.Mode == admissioncontract.ModeInspect && identity.State != admissioncontract.StateVerified {
		g.refuse("identity", "live")
	}
	if err := g.checkResourceIdentity(identity); err != nil {
		return nil, err
	}
	if err := g.bindSources(hooks); err != nil {
		return nil, err
	}
	g.holdPlan = g.eligibleHoldPlan()
	if err := g.transform(wf, r, identity, hooks); err != nil {
		return nil, err
	}
	return g.finish(wf, r, identity)
}
func prepareDocument(r admissioncontract.PrepareRequest) (*wfv1.Workflow, error) {
	if err := validateRequest(r); err != nil {
		return nil, err
	}
	wf, err := Decode(r.Document)
	if err != nil {
		return nil, err
	}
	if err := applyDefaults(wf, r); err != nil {
		return nil, err
	}
	return wf, nil
}
func (p Policy) admitRequest(ctx context.Context, wf *wfv1.Workflow, r admissioncontract.PrepareRequest, hooks admissioncontract.PrepareHooks) *graph {
	g := newGraph(ctx, wf, r, p)
	g.loginAudience = hooks.LoginAudience
	g.offlineValidation = hooks.OfflineValidation
	g.build()
	g.admit()
	return g
}
func (g *graph) transform(wf *wfv1.Workflow, r admissioncontract.PrepareRequest, identity admissioncontract.IdentityEvidence, hooks admissioncontract.PrepareHooks) error {
	if err := g.stampReleaseFacts(wf, hooks); err != nil {
		return err
	}
	bindIdentity(wf, identity)
	g.stampParameters(wf, r)
	if hooks.TransformNative == nil {
		return nil
	}
	input := admissioncontract.TransformInput{Plan: g.holdPlan, Mode: r.Mode, Identity: identity, CredentialSteps: map[string]string{}, SecretGrants: g.secretGrants, SourceBindings: g.bindings, Defaults: r.Defaults}
	for _, n := range g.nodes {
		if g.nodeUsesCredentials(n) {
			input.CredentialSteps[n.key] = g.credentialStep(n)
		}
	}
	input = input.Clone()
	if err := hooks.TransformNative(g.ctx, wf, input); err != nil {
		return err
	}
	for i := range g.holdPlan.Entries {
		g.holdPlan.Entries[i].HoldArmed = input.Plan.Entries[i].HoldArmed
	}
	return nil
}
func (g *graph) finish(wf *wfv1.Workflow, r admissioncontract.PrepareRequest, identity admissioncontract.IdentityEvidence) (*admissioncontract.PreparedAdmission, error) {
	final := g.finalGraph(wf)
	if err := final.err(); err != nil {
		return nil, err
	}
	if g.inventory.usesSource {
		size, err := specsize.Submitted(wf)
		if err != nil || size > admissioncontract.MaxSourceSpecBytes {
			return nil, refusal("spec", "source_size")
		}
	}
	final.inventory = final.sources()
	if err := final.validateBindings(r); err != nil {
		return nil, err
	}
	if err := final.nativeSchema(wf, identity); err != nil {
		if !g.collect(err) {
			return nil, err
		}
	}
	g.findings = append(g.findings, final.findings...)
	if err := g.err(); err != nil {
		return nil, err
	}
	view := preparedView(r, wf, identity, g)
	view.Plan = frozenHoldPlan(final.plan(), g.holdPlan)
	return admissioncontract.Seal(view)
}
func validateRequest(r admissioncontract.PrepareRequest) error {
	if !requestModeValid(r) {
		return refusal("request", "binding")
	}
	if len(r.SelectedFile) > workflowpolicy.MaxFileBytes {
		return refusal("selected_file", "file_size")
	}
	if err := workflowpolicy.ValidateFile(r.SelectedFile); err != nil {
		return refusal("selected_file", "binding")
	}
	if !scheduleValid(string(r.Trigger), r.ScheduleEntry) {
		return refusal("schedule_entry", "schedule")
	}
	if r.Mode == admissioncontract.ModeInspect {
		return nil
	}
	if r.RunID == "" || r.Ref == "" || r.FileBlob == "" || r.SourceReceipt.Validate(r.Document) != nil {
		return refusal("source_receipt", "binding")
	}
	receipt := r.SourceReceipt
	if !receiptMatches(r, receipt) {
		return refusal("source_receipt", "binding")
	}
	return nil
}
func applyDefaults(wf *wfv1.Workflow, r admissioncontract.PrepareRequest) error {
	raw := r.Defaults.WorkflowDefaults
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > admissioncontract.MaxDocumentBytes {
		return refusal("defaults", "defaults_size")
	}
	var base wfv1.Workflow
	if json.Unmarshal(raw, &base) != nil {
		return refusal("defaults", "document")
	}
	canonical, err := sourcecontract.CanonicalJSON(raw)
	if err != nil || r.Defaults.Digest != sourcecontract.Digest(canonical) {
		return refusal("defaults.digest", "binding")
	}
	if defaults.MergeTo(&base, wf) != nil {
		return refusal("defaults", "native")
	}
	return nil
}
func verifyIdentity(ctx context.Context, r admissioncontract.PrepareRequest, hooks admissioncontract.PrepareHooks) (admissioncontract.IdentityEvidence, error) {
	e := admissioncontract.IdentityEvidence{Namespace: r.IdentityRequest.Namespace, Pipeline: admissioncontract.AccountEvidence{Name: r.IdentityRequest.PipelineAccount}, State: admissioncontract.StateLiveRequired}

	if r.IdentityRequest.ResourceAccount != "" {
		e.Resource = &admissioncontract.AccountEvidence{Name: r.IdentityRequest.ResourceAccount}
	}
	if hooks.VerifyIdentities != nil {
		var err error
		e, err = hooks.VerifyIdentities(ctx, r.IdentityRequest)
		if err != nil {
			return e, err
		}
	}
	if r.Mode == admissioncontract.ModeInspect && e.State != admissioncontract.StateVerified {
		return e, nil
	}
	return e, validateIdentity(r.IdentityRequest, e, r.Mode == admissioncontract.ModeSubmit)
}
func (g *graph) finalGraph(wf *wfv1.Workflow) *graph {
	p := g.policy
	r := g.request
	r.Templates = g.snapshots
	p.TemplateGetter = func(ctx context.Context, name string) (*wfv1.WorkflowTemplate, error) {
		if g.objects[name] == nil {
			return nil, nil
		}
		return retainedTemplates(g.objects).Get(ctx, name)
	}
	if g.offline() {
		p.TemplateGetter = nil
	}
	final := newGraph(g.ctx, wf, r, p)
	final.offlineValidation = g.offlineValidation
	final.attempts = g.attempts
	final.bindings = g.bindings
	final.build()
	final.secretGrants = g.secretGrants
	return final
}
func preparedView(r admissioncontract.PrepareRequest, wf *wfv1.Workflow, e admissioncontract.IdentityEvidence, g *graph) admissioncontract.PreparedView {
	return admissioncontract.PreparedView{Workflow: wf, Repository: r.Repository, RunID: r.RunID, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA, Trigger: r.Trigger, ScheduleEntry: r.ScheduleEntry, SelectedFile: r.SelectedFile, FileBlob: r.FileBlob, SourceReceipt: r.SourceReceipt, SourceBindings: g.bindings, Defaults: r.Defaults, Templates: g.snapshots, Identity: e, Grants: r.Grants, Findings: g.findings}
}
func (g *graph) validateBindings(r admissioncontract.PrepareRequest) error {
	declarations := g.inventory.boundInputs()
	if r.Mode == admissioncontract.ModeInspect && len(g.bindings) == 0 {
		if len(declarations) > 0 {
			g.refuse("source_bindings", "live")
		}
		return nil
	}
	if len(declarations) != len(g.bindings) {
		return refusal("source_bindings", "binding")
	}
	for _, b := range g.bindings {
		if !matchesDeclaration(b, declarations) {
			return refusal("source_bindings", "binding")
		}
	}
	return nil
}
func bindingMatches(b sourcecontract.Binding, r admissioncontract.PrepareRequest, declarations []SourceInput) bool {
	if b.Validate() != nil {
		return false
	}
	if b.Descriptor.Kind == sourcecontract.Main && !mainBindingMatches(b, r) {
		return false
	}
	return matchesDeclaration(b, declarations)
}
func matchesDeclaration(b sourcecontract.Binding, declarations []SourceInput) bool {
	for _, d := range declarations {
		if declarationMatches(b, d) {
			return true
		}
	}
	return false
}
func declarationMatches(b sourcecontract.Binding, d SourceInput) bool {
	return d.Template == b.Template && d.Artifact == b.Artifact && d.Path == b.Path && d.Descriptor == b.Descriptor
}
func (g *graph) nativeSchema(wf *wfv1.Workflow, identity admissioncontract.IdentityEvidence) error {
	if g.offline() && g.unresolvedReferences() {
		g.refuse("native_schema", "live")
	}
	validation := wf
	if g.offline() && identity.State != admissioncontract.StateVerified {
		validation = wf.DeepCopy()
		if validation.Spec.Executor == nil {
			validation.Spec.Executor = &wfv1.ExecutorConfig{}
		}
		validation.Spec.Executor.ServiceAccountName = "unverified"
	}
	return validateNativeSchema(g.ctx, validation, admissioncontract.PrepareRequest{Templates: g.snapshots}, g)
}
func (g *graph) unresolvedReferences() bool {
	for _, n := range g.nodes {
		for _, c := range edges(n) {
			if c.target == nil && len(c.candidates) == 0 {
				return true
			}
		}
	}
	return false
}
func receiptMatches(r admissioncontract.PrepareRequest, s sourcecontract.Receipt) bool {
	return s.Revision.Repository == r.Repository && s.Revision.Commit == r.CommitSHA && s.ObjectSHA == r.ObjectSHA && s.File == r.SelectedFile && s.Blob == r.FileBlob
}

func requestModeValid(r admissioncontract.PrepareRequest) bool {
	return r.Mode.Valid() && r.Trigger.Valid() && (r.IdentityRequest.Mode == "" || r.IdentityRequest.Mode == r.Mode)
}

func mainBindingMatches(b sourcecontract.Binding, r admissioncontract.PrepareRequest) bool {
	if b.Revision != r.SourceReceipt.Revision {
		return false
	}
	own := b.Export.Ref
	return own == (sourcecontract.Ref{}) || (own.Name == r.Ref && own.ObjectSHA == r.ObjectSHA)
}

func (g *graph) bindSources(hooks admissioncontract.PrepareHooks) error {
	declarations := g.inventory.boundInputs()
	if hooks.BindSource == nil {
		if len(declarations) > 0 && !g.offline() {
			return refusal("source_bindings", "binding")
		}
		return nil
	}
	g.prepareSources(declarations, hooks)
	var failures []error
	for _, d := range declarations {
		if err := g.ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		binding, err := hooks.BindSource(g.ctx, d.Descriptor)
		if err != nil {
			where := d.Template + ".inline"
			if n := g.byKey[d.Template]; n != nil {
				where = n.path
			}
			failures = append(failures, fmt.Errorf("%s.inputs.artifacts[%q]: %w", where, d.Artifact, err))
			continue
		}
		binding = binding.Clone()
		binding.Template, binding.Artifact, binding.Path = d.Template, d.Artifact, d.Path
		if len(binding.Export.Tags) > sourcecontract.MaxRefs {
			failures = append(failures, refusal("source_bindings["+d.Template+"/"+d.Artifact+"]", "tag_count"))
			continue
		}
		if !bindingMatches(binding, g.request, []SourceInput{d}) {
			failures = append(failures, refusal("source_bindings["+d.Template+"/"+d.Artifact+"]", "binding"))
			continue
		}
		g.bindings = append(g.bindings, binding)
	}
	return errors.Join(failures...)
}

func (g *graph) prepareSources(declarations []SourceInput, hooks admissioncontract.PrepareHooks) {
	if hooks.PrepareSources == nil || len(declarations) == 0 {
		return
	}
	descriptors := make([]sourcecontract.Descriptor, len(declarations))
	for i, d := range declarations {
		descriptors[i] = d.Descriptor
	}
	hooks.PrepareSources(g.ctx, descriptors)
}
