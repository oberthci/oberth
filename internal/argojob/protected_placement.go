package argojob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func (p *ExecutionPlacement) selectProtectedRequest(config Config, r Request) error {
	d := p.document
	name := d.NormalWorkflow
	if r.TrustedKVM != nil {
		name = d.PreflightWorkflow
	}
	digest := sha256.Sum256(r.Source)
	if config.Namespace != d.Namespace || config.PipelineServiceAccount != d.ServiceAccount || canonicalRepoKey(r.UpstreamName, r.UpstreamOrg, r.Repo) != d.Repository || r.RepositoryID != d.RepositoryID || r.SHA != d.SHA || r.Ref != d.Ref || r.PipelineFile != d.PipelineFile || r.RunID != d.ParentRunID || r.Name != name || r.Trigger != pipeline.TriggerCI || r.effectiveRunTrigger() != argoworkflow.RunTriggerBranch || hex.EncodeToString(digest[:]) != d.SourceDigest || (len(r.Fragments) != 0 && d.GraphDigest == "") || r.releasePreflight || r.omitRequestDefaults || r.omitStepUsage {
		return errors.New("argojob: request differs from protected placement authority")
	}
	if d.GraphDigest != "" {
		if err := p.selectPlacementFragments(r); err != nil {
			return err
		}
	}

	if r.Capability == nil || r.Capability.Validate() != nil || protectedtests.CapabilityDigest(*r.Capability) != d.ProtectedPolicy.CapabilityDigest || config.TrustedKVMGroup != d.ProtectedPolicy.SupplementalGroup {
		return errors.New("argojob: protected placement capability or device group differs")
	}
	if r.TrustedKVM != nil {
		a := r.GenericProtected
		if a == nil || validateGenericProtectedRequest(r) != nil || d.ProtectedPolicy.Match(*a) != nil || a.Intent.RunID != d.ParentRunID || a.Intent.WorkflowName != name || a.Intent.Namespace != d.Namespace || a.Intent.CandidateSHA != d.SHA || a.Intent.WorkflowSpecDigest != d.SourceDigest || a.Intent.Deadline.After(d.Expires) {
			return errors.New("argojob: dynamic host intent differs from protected placement")
		}
	}
	return nil
}

// ProtectedAdmission seals the already validated public executor snapshot.
// Reservation remains a separate atomic database operation owned by the app.
func ProtectedAdmission(config Config, r Request) (*protectedtests.PlacementAdmission, error) {
	if !config.ExecutionPlacement.Protected() {
		return nil, nil
	}
	config.applyDefaults()
	if err := config.ExecutionPlacement.selectRequest(config, r); err != nil {
		return nil, err
	}
	if r.GenericProtected == nil {
		return nil, errors.New("argojob: placed preflight requires generic runtime")
	}
	placement, err := json.Marshal(config.ExecutionPlacement)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	if config.ExecutionPlacement.document.GraphDigest != "" {
		a, err := protectedtests.NewFragmentPlacementAdmission(placement, body, *r.GenericProtected, r.Source, r.Fragments)
		return &a, err
	}
	a, err := protectedtests.NewPlacementAdmission(placement, body, *r.GenericProtected)
	return &a, err
}

// ProtectedDeadline bounds a new intent before it is journaled. It never edits
// a retained intent and cannot extend either the run cap or operator expiry.
func (p *ExecutionPlacement) ProtectedDeadline(now time.Time, deadline time.Time) (time.Time, error) {
	if !p.Protected() {
		return deadline, nil
	}
	if err := p.ValidateAdmission(); err != nil {
		return time.Time{}, err
	}
	cap := now.Add(time.Duration(p.document.ProtectedPolicy.MaxPhaseSeconds) * time.Second)
	if deadline.After(cap) {
		deadline = cap
	}
	if deadline.After(p.document.Expires) {
		deadline = p.document.Expires
	}
	if !deadline.After(now) {
		return time.Time{}, errors.New("argojob: protected placement expired")
	}
	return deadline, nil
}

func (p *ExecutionPlacement) ValidateProtectedPolicy(baseline string, contract *protectedtests.Contract, recipe protectedtests.Recipe) error {
	if !p.Protected() {
		return nil
	}
	policy := p.document.ProtectedPolicy
	if policy == nil || policy.Validate() != nil || baseline != policy.BaselineSHA || contract == nil || protectedtests.ContractDigest(contract) != protectedtests.ContractDigest(&policy.Contract) || recipe != policy.Recipe {
		return errors.New("argojob: protected contract differs from placement policy")
	}
	return nil
}

// ParseExecutionPlacementForStartup permits expired protected custody to reach
// cleanup. Every admission/allocation path still validates its original expiry.
func ParseExecutionPlacementForStartup(body []byte) (*ExecutionPlacement, error) {
	p, err := decodeExecutionPlacement(body)
	if err != nil {
		return nil, err
	}
	if !p.Protected() {
		return ParseExecutionPlacement(body)
	}
	return p, nil
}
