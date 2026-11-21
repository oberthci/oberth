package app

import (
	"context"
	"encoding/json"
	"fmt"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"slices"
	"strconv"
)

// AuthorityConfig connects the current grant snapshot and native namespace to
// admission. Pushes and schedules ensure accounts; checks read existing UIDs.
type AuthorityConfig struct {
	ArgoNamespace      string
	ServerNamespace    string
	Namespace          string
	SecretstoreAddress string
	ImagePrefixes      []string
	Defaults           runtimeconfig.WorkflowDefaultsSnapshot
	Grants             func(context.Context, []string) (stateport.AuthoritySnapshot, error)
	Templates          func(context.Context, string) (*wf.WorkflowTemplate, error)
	EnsureAccount      func(context.Context, string) error
}

type AuthorityReader struct{ config AuthorityConfig }

func NewAuthority(c AuthorityConfig) (*AuthorityReader, error) {
	if c.Grants == nil || c.Templates == nil || c.EnsureAccount == nil || c.Namespace == "" || c.ServerNamespace == "" || c.ArgoNamespace == "" {
		return nil, refusal("config", nil)
	}
	c.ImagePrefixes = slices.Clone(c.ImagePrefixes)
	c.Defaults = c.Defaults.Clone()
	return &AuthorityReader{config: c}, nil
}

type authoritySnapshot struct {
	request  *admissioncontract.PrepareRequest
	policy   *argoworkflow.Policy
	revision string
}

func (a *AuthorityReader) Load(ctx context.Context, s Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
	o := a.config
	snap, err := o.Grants(ctx, []string{s.Repository})
	if err != nil {
		return admissioncontract.PrepareRequest{}, argoworkflow.Policy{}, err
	}
	p := argoworkflow.Policy{ServerNamespace: o.ServerNamespace, ArgoNamespace: o.ArgoNamespace, Release: s.Release, SecretstoreAddress: o.SecretstoreAddress, ImagePrefixes: slices.Clone(o.ImagePrefixes), PVCClaims: map[string]bool{}, TemplateGetter: o.Templates}
	kind := identityname.PipelineCI
	if s.Release {
		kind = identityname.Pipeline
	}
	name, err := identityname.Name(identityname.Key{Repository: s.Repository}, kind)
	if err != nil {
		return admissioncontract.PrepareRequest{}, p, err
	}
	q := admissioncontract.PrepareRequest{Defaults: o.Defaults.Clone(), IdentityRequest: admissioncontract.IdentityRequest{Namespace: o.Namespace, PipelineAccount: name}}
	for _, g := range snap.Grants {
		if g.Revoke || g.Dismiss || g.Repository != s.Repository || g.File != "" && g.File != s.File {
			continue
		}
		if err := (authoritySnapshot{request: &q, policy: &p, revision: strconv.FormatInt(snap.Revision, 10)}).apply(ctx, g); err != nil {
			return q, p, err
		}
	}
	if s.executes() {
		return q, p, a.prepareAccounts(ctx, q, p.Release)
	}

	return q, p, nil
}
func (a authoritySnapshot) apply(ctx context.Context, g stateport.GrantChange) error {
	q, p, revision := a.request, a.policy, a.revision
	if g.Kind == stateport.GrantCapability {
		if err := p.LoadCapability(ctx, q.IdentityRequest.Namespace, g.Capability); err != nil {
			return err
		}
		q.IdentityRequest.ResourceAccount = p.Capability.ServiceAccount
		return nil
	}

	kind := map[stateport.GrantKind]string{stateport.GrantStorePath: "secret", stateport.GrantSecret: "cluster-secret", stateport.GrantConfigMap: "configmap", stateport.GrantImagePull: "image-pull-secret", stateport.GrantPVC: "pvc", stateport.GrantDevice: "device", stateport.GrantAudience: "identity"}[g.Kind]
	if kind == "" {
		return fmt.Errorf("unknown authority kind %q", g.Kind)
	}
	evidence := admissioncontract.GrantEvidence{Repository: g.Repository, File: g.File, Step: g.Step, Path: g.Path, Kind: kind, Revision: revision, State: admissioncontract.StateVerified}
	if g.Kind == stateport.GrantAudience {
		evidence.Path, evidence.Audience = "identity", g.Path
	}
	if g.Kind == stateport.GrantPVC {
		p.PVCClaims[g.Path] = p.PVCClaims[g.Path] || g.Tier == "release" && p.Release || g.Tier == "ci" && !p.Release
	}
	q.Grants = append(q.Grants, evidence)
	if g.Kind == stateport.GrantStorePath {
		_, err := stepAuthority(evidence, p.Release)
		return err
	}
	return nil
}
func stepAuthority(evidence admissioncontract.GrantEvidence, release bool) (string, error) {
	kind := identityname.StepCI
	if release {
		kind = identityname.Step
	}
	return identityname.Name(evidence.Key(), kind)
}

func (o *AuthorityReader) prepareAccounts(ctx context.Context, q admissioncontract.PrepareRequest, release bool) error {
	names := []string{q.IdentityRequest.PipelineAccount}
	for _, grant := range q.Grants {
		if grant.Kind != "secret" && (!release || grant.Kind != "identity") {
			continue
		}
		name, err := stepAuthority(grant, release)
		if err != nil {
			return err
		}
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		if err := o.config.EnsureAccount(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// ValidateCapability uses admission's current policy for an operator proposal.
func (a *AuthorityReader) ValidateCapability(ctx context.Context, q apicontract.Capability) error {
	raw, err := json.Marshal(q)
	if err != nil {
		return err
	}
	p := argoworkflow.Policy{ServerNamespace: a.config.ServerNamespace, ArgoNamespace: a.config.ArgoNamespace}
	return p.LoadCapability(ctx, a.config.Namespace, raw)
}

func (s Scope) executes() bool {
	return s.Mode == admissioncontract.ModeSubmit || s.Trigger == string(model.TriggerSchedule)
}
