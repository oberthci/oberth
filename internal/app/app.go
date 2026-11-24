package app

import (
	"context"
	"errors"
	"net/url"
	"sync"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

type Submitter struct {
	d                Deps
	policy           workflowpolicy.SourcePolicy
	mu               sync.Mutex
	receives         map[string]acceptance
	ctx              context.Context
	cancel           context.CancelFunc
	closing, started bool
	tasks            sync.WaitGroup
}

func New(d Deps) (*Submitter, error) {
	u, err := url.Parse(d.PublicURL)
	if !dependencies(d) || err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
		return nil, refusal("config", nil)
	}
	policy, err := sourcePolicy(d.SourcePolicy)
	if err = errors.Join(err, d.Routes.Validate()); err != nil {
		return nil, err
	}
	d.Routes, d.SourcePolicy = d.Routes.Clone(), nil
	ctx, cancel := context.WithCancel(d.Lifetime)
	return &Submitter{d: d, policy: policy, receives: map[string]acceptance{}, ctx: ctx, cancel: cancel}, nil
}

// Close refuses new work and drains submissions until termination cancels calls.
func (s *Submitter) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.tasks.Wait(); close(done) }()
	defer s.cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Submitter) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return refusal("closed", nil)
	}
	s.started = true
	s.tasks.Add(1)
	return nil
}

func (s *Submitter) StartRun(ctx context.Context, q service.StartRequest) (apicontract.Receipt, error) {
	if q.Request.ReceiveID == "" {
		return apicontract.Receipt{}, refusal("request", nil)
	}
	if err := s.begin(); err != nil {
		return apicontract.Receipt{}, err
	}
	defer s.tasks.Done()
	result, err := s.deliver(ctx, Receive{ID: q.Request.ReceiveID}, func(ctx context.Context) (acceptance, error) { return s.prepareStart(ctx, q) })
	if err != nil {
		return apicontract.Receipt{}, err
	}
	if len(result.Outcomes) == 0 {
		return apicontract.Receipt{}, refusal("request", nil)
	}
	o := result.Outcomes[0]
	receipt := apicontract.Receipt{ID: o.RunID, State: o.State}
	if o.Problem != nil {
		receipt.Warning = o.Problem.Remedy()
	}
	if q.Request.Assist && !assistEnabled {
		if receipt.Warning != "" {
			receipt.Warning += "\n"
		}
		receipt.Warning += messages()["assist"].remedy
	}
	return receipt, nil
}

func (s *Submitter) submissionContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}
func dependencies(d Deps) bool {
	return d.SourceTimeout > 0 && d.Lifetime != nil && d.Source != nil && d.Authority != nil && d.Runs != nil && d.Service != nil && d.Native != nil && d.Keeper != nil && d.Deadline != nil && d.Resolver != nil
}

func (s *Submitter) prepareStart(ctx context.Context, q service.StartRequest) (acceptance, error) {
	r, err := s.d.Resolver.ResolveStart(ctx, q)
	if err != nil {
		return acceptance{}, err
	}
	r.Assist = q.Request.Assist
	run, err := s.admit(ctx, r, s.d.Source, admitOptions{mode: admissioncontract.ModeSubmit, reason: q.Reason})
	if err != nil {
		return acceptance{}, err
	}
	request := q.Request.Clone()
	request.RequestedFile = run.Request.RequestedFile
	run.Request, run.RerunOf, run.PromotionID = request, q.RerunOf, q.PromotionID
	return acceptance{BatchAcceptance: stateport.BatchAcceptance{ReceiveID: run.Request.ReceiveID, Repository: run.Request.Repo, Actor: run.Request.Actor.Clone(), Runs: []stateport.Acceptance{run}}, Result: PushResult{Outcomes: []Outcome{{Ref: r.Ref.Name, RunID: r.RunID, State: "accepted"}}}}, nil
}
