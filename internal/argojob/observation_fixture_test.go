package argojob_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func memoryAdmission(t *testing.T, p *admissioncontract.PreparedAdmission) *statefake.Runs {
	t.Helper()
	v := p.View()
	commit := v.CommitSHA
	r := model.CIRequest{ReceiveID: "native", Repo: v.Repository, Ref: v.Ref, ObjectSHA: v.ObjectSHA, CommitSHA: &commit, Trigger: v.Trigger, ScheduleEntry: v.ScheduleEntry}
	r.Actor.Identity, r.Actor.AuthMethod = "operator", "cert"
	return memoryAdmissionFor(t, p, r)
}
func memoryAdmissionFor(t *testing.T, p *admissioncontract.PreparedAdmission, r model.CIRequest) *statefake.Runs {
	t.Helper()
	s := statefake.NewRuns(compositionClock)
	s.SeedRepository(stateport.Repository{ID: 1, Canonical: r.Repo, Upstream: "forge", Name: "org/repo"})
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: p}}})
	compositionOK(t, err)
	return s
}
func observationFixture(t *testing.T) (argojob.Config, *fake.Workflows, nativebinding.RunBinding) {
	t.Helper()
	p, _, f := fixture(t)
	s := memoryAdmission(t, p)
	config := argojob.Config{Workflows: f, Runs: s, Progress: s, Now: compositionClock, Cache: &fake.Cache{}, Reporter: &reportSpy{}}
	b, err := argojob.New(config).Create(t.Context(), p)
	compositionOK(t, err)
	return config, f, b
}

// reportSpy models idempotent service facts and counts every delivery attempt.
// Durable acceptance and evidence use the owner's fake or the real Store.
type reportSpy struct {
	mu           sync.Mutex
	HeldCalls    int
	FinishedFunc func(context.Context, string, argojob.Observation) error
	HeldFunc     func(context.Context, model.HeldNotice) error
	Holds        []model.HeldNotice
}

func (r *reportSpy) Finished(ctx context.Context, id string, o argojob.Observation) error {
	if r.FinishedFunc != nil {
		return r.FinishedFunc(ctx, id, o)
	}
	return nil
}
func (r *reportSpy) Held(ctx context.Context, h model.HeldNotice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.HeldCalls++
	if slices.Contains(r.Holds, h) {
		return nil
	}
	if r.HeldFunc != nil {
		if err := r.HeldFunc(ctx, h); err != nil {
			return err
		}
	}
	r.Holds = append(r.Holds, h)
	return nil
}

// progressProbe forwards accepted writes to the owner while observing order or
// injecting a specific port refusal.
type progressProbe struct {
	*statefake.Runs
	advance  func(context.Context, string) error
	save     func(context.Context, string, []stepmodel.RunStep) error
	begin    func(context.Context, stateport.Action, stateport.EvidenceStart) (int64, error)
	complete func(context.Context, stateport.Action, stateport.EvidenceCompletion) error
}

func (p *progressProbe) SaveSteps(ctx context.Context, id string, s []stepmodel.RunStep) error {
	if p.save != nil {
		return p.save(ctx, id, s)
	}
	return p.Runs.SaveSteps(ctx, id, s)
}
func (p *progressProbe) BeginEvidence(ctx context.Context, a stateport.Action, in stateport.EvidenceStart) (int64, error) {
	if p.begin != nil {
		return p.begin(ctx, a, in)
	}
	return p.Runs.BeginEvidence(ctx, a, in)
}
func (p *progressProbe) CompleteEvidence(ctx context.Context, a stateport.Action, in stateport.EvidenceCompletion) error {
	if p.complete != nil {
		return p.complete(ctx, a, in)
	}
	return p.Runs.CompleteEvidence(ctx, a, in)
}

func requestForProgress() model.CIRequest {
	r := model.CIRequest{ReceiveID: "native", Repo: "forge/org/repo", Ref: "refs/heads/topic", ObjectSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CommitSHA: new("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Trigger: model.TriggerBranch}
	r.Actor.Identity, r.Actor.AuthMethod = "operator", "cert"
	return r
}

func (p *progressProbe) AdvanceNative(ctx context.Context, id string) error {
	if p.advance != nil {
		return p.advance(ctx, id)
	}
	return p.Runs.AdvanceNative(ctx, id)
}
