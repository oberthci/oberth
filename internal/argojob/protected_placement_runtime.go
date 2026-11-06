package argojob

import (
	"context"
	"encoding/json"
	"errors"

	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/protectedtests"
)

type ProtectedPlacementJournal interface {
	BindPlacementSource(context.Context, string, string, string) error
	PlacementSourceUID(context.Context, string, string) (string, error)
	PlacementAdmissionForWorkflow(context.Context, string) (*protectedtests.PlacementAdmission, error)
	PlacementAdmission(context.Context, string) (*protectedtests.PlacementAdmission, error)
	GenericAdmission(context.Context, string) (*protectedtests.GenericAdmission, error)
	CheckGenericPreflightAuthority(context.Context, string) error
	KVMPreflight(context.Context, string) (protectedtests.Execution, error)
}

// WithProtectedPlacementJournal installs only trusted server dependencies. The
// transport stays in memory and is never serialized into custody or workloads.
func (c *Controller) WithProtectedPlacementJournal(j ProtectedPlacementJournal, transport *rest.Config) *Controller {
	c.placementJournal = j
	if transport != nil {
		c.placementTransport = rest.CopyConfig(transport)
	}
	if c.seeder != nil {
		c.seeder.placementCheck = c.checkPlacedAllocation
		c.seeder.placementSource = c.bindPlacedSource
	}
	return c
}
func (c *Controller) WithProtectedPlacementAuthority(check func(context.Context, protectedtests.GenericAdmission) error) {
	c.placementAuthority = check
}

func (c *Controller) checkPlacedAllocation(ctx context.Context, name string) error {
	p := c.config.ExecutionPlacement
	if p != nil && p.observationOnly {
		return errors.New("argojob: retained placement cannot authorize allocation or receipt")
	}
	if c.placementJournal == nil {
		if p.Protected() {
			return errors.New("argojob: protected placement journal unavailable")
		}
		return nil
	}
	// A primary placement binds one run. Without it, any recorded placed phase
	// must still refuse fallback; callers supply the canonical run where needed.
	if !p.Protected() {
		retained, err := c.placementJournal.PlacementAdmissionForWorkflow(ctx, name)
		if err != nil {
			return err
		}
		if retained != nil {
			return errors.New("argojob: retained protected phase cannot fall back after policy removal")
		}
		return nil
	}
	a, err := c.placementJournal.PlacementAdmission(ctx, p.document.ParentRunID)
	if err != nil {
		return err
	}
	if a == nil {
		return errors.New("argojob: protected placement has not been journaled")
	}
	runtime, err := c.placementJournal.GenericAdmission(ctx, p.document.ParentRunID)
	if err != nil {
		return err
	}
	if runtime == nil || a.Validate(*runtime) != nil {
		return errors.New("argojob: protected placement runtime unavailable")
	}
	scope, err := a.Scope()
	if err != nil {
		return err
	}
	if name != scope.NormalWorkflow && name != scope.PreflightWorkflow {
		return errors.New("argojob: allocation phase differs from placement")
	}
	body, err := json.Marshal(c.config)
	if err != nil {
		return err
	}
	if string(body) != string(a.Config) {
		return errors.New("argojob: current executor configuration differs from placement custody")
	}
	if err = p.VerifyTarget(c.placementTransport); err != nil {
		return err
	}
	if err = p.ValidateAdmission(); err != nil {
		return err
	}
	if err = c.placementJournal.CheckGenericPreflightAuthority(ctx, scope.RunID); err != nil {
		return err
	}
	if err = c.verifyCapabilityGrant(ctx, runtime.Intent.RepoID, &runtime.Capability); err != nil {
		return err
	}
	if c.placementAuthority == nil {
		return errors.New("argojob: protected source authority unavailable")
	}
	if err = c.placementAuthority(ctx, *runtime); err != nil {
		return err
	}
	if name == scope.NormalWorkflow {
		execution, err := c.placementJournal.KVMPreflight(ctx, scope.RunID)
		if err != nil {
			return err
		}
		if execution.Receipt == nil || !execution.Cleaned || execution.Refusal != nil {
			return errors.New("argojob: normal phase requires protected receipt and completed cleanup")
		}
	}
	return p.verifyEnvironment(ctx, c.kube, c.config)
}

func (c *Controller) checkPlacedRequest(ctx context.Context, r Request) error {
	if c.placementJournal != nil {
		a, err := c.placementJournal.PlacementAdmission(ctx, r.RunID)
		if err != nil {
			return err
		}
		if a != nil {
			if !c.config.ExecutionPlacement.Protected() {
				return errors.New("argojob: retained protected placement cannot fall back to another executor policy")
			}
			if r.TrustedKVM != nil && a.IntentSHA256 != protectedtests.IntentDigest(*r.TrustedKVM) {
				return errors.New("argojob: host intent differs from placement custody")
			}
		}
	}
	return c.checkPlacedAllocation(ctx, r.Name)
}

func (s *SourceSeeder) checkPlacedAllocation(ctx context.Context, name string) error {
	if s.config.ExecutionPlacement != nil && s.config.ExecutionPlacement.observationOnly {
		return errors.New("argojob: retained placement cannot seed")
	}
	if s.placementCheck != nil {
		return s.placementCheck(ctx, name)
	}
	if s.config.ExecutionPlacement.Protected() {
		return errors.New("argojob: protected seeder journal unavailable")
	}
	return nil
}

// ProtectedRecoveryController reconstructs the original same-instance owner.
// Its private observation marker is rejected by every allocation/receipt path.
// A different authenticated endpoint or cluster retains the obligation.
func (c *Controller) ProtectedRecoveryController(ctx context.Context, runID string) (*Controller, error) {
	if c.config.ExecutionPlacement != nil && c.config.ExecutionPlacement.observationOnly {
		return c, nil
	}
	if c.placementJournal == nil {
		if c.config.ExecutionPlacement.Protected() {
			return nil, errors.New("argojob: protected recovery journal unavailable")
		}
		return c, nil
	}
	a, err := c.placementJournal.PlacementAdmission(ctx, runID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return c, nil
	}
	runtime, err := c.placementJournal.GenericAdmission(ctx, runID)
	if err != nil {
		return nil, err
	}
	if runtime == nil || a.Validate(*runtime) != nil {
		return nil, errors.New("argojob: retained placement runtime differs")
	}
	var config Config
	if err = json.Unmarshal(a.Config, &config); err != nil {
		return nil, err
	}
	if !config.ExecutionPlacement.Protected() {
		return nil, errors.New("argojob: retained protected placement missing")
	}
	if err = config.ExecutionPlacement.VerifyTarget(c.placementTransport); err != nil {
		return nil, err
	}
	config.ExecutionPlacement.observationOnly = true
	if err = config.ExecutionPlacement.verifyEnvironment(ctx, c.kube, config); err != nil {
		return nil, err
	}
	recovered, err := NewController(c.workflows, c.kube, config)
	if err != nil {
		return nil, err
	}
	recovered.placementJournal = c.placementJournal
	recovered.placementTransport = c.placementTransport
	recovered.interval = c.interval
	recovered.readGrace = c.readGrace
	recovered.logs = c.logs
	recovered.followLogs = c.followLogs
	recovered.logIdentity = c.logIdentity
	recovered.logPod = c.logPod
	recovered.vmi = c.vmi
	// No grant loader, policy callback or source seeder is attached. This object
	// cannot obtain create authority by being passed to an ordinary entry point.
	return recovered, nil
}

// CheckProtectedPlacementAuthority reuses the actual allocation verifier at
// scheduler delivery/publication. A stored receipt cannot attest a live target.
func (c *Controller) CheckProtectedPlacementAuthority(ctx context.Context, runID string) error {
	p := c.config.ExecutionPlacement
	if !p.Protected() || p.document.ParentRunID != runID {
		return errors.New("argojob: current protected publication placement differs")
	}
	return c.checkPlacedAllocation(ctx, p.document.NormalWorkflow)
}

// bindPlacedSource cannot infer ownership of a preexisting unjournaled claim.
func (c *Controller) bindPlacedSource(ctx context.Context, workflow, uid string, created bool) error {
	if err := c.checkPlacedAllocation(ctx, workflow); err != nil {
		return err
	}
	p := c.config.ExecutionPlacement
	if !p.Protected() || c.placementJournal == nil || uid == "" {
		return errors.New("argojob: protected source custody unavailable")
	}
	if !created {
		prior, err := c.placementJournal.PlacementSourceUID(ctx, p.document.ParentRunID, workflow)
		if err != nil {
			return err
		}
		if prior == "" || prior != uid {
			return errors.New("argojob: preexisting source claim lacks exact custody")
		}
		return nil
	}
	return c.placementJournal.BindPlacementSource(ctx, p.document.ParentRunID, workflow, uid)
}
