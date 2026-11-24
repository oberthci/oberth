package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"slices"
)

type Outcome struct {
	Ref, RunID, State, File, WatchURL string
	Problem                           *Refusal
}
type PushResult struct {
	Outcomes      []Outcome
	AssistIgnored bool
	assistNotUsed []assistRefusal
}
type PushRefusal struct{ Problems []*Refusal }

func (e *PushRefusal) Error() string {
	lines := []string{messages()["push"].reason}
	for _, p := range e.Problems {
		lines = append(lines, p.Error())
	}
	return strings.Join(lines, "\n")
}
func (e *PushRefusal) Unwrap() []error {
	out := make([]error, len(e.Problems))
	for i, p := range e.Problems {
		out[i] = p
	}
	return out
}

// Gate checks every pushed ref against quarantine before any ref moves.
// Delivering the release tag is its last step: once the tag is on the forge,
// the gate passes, since a refusal then would spend the version with no run.
func (s *Submitter) Gate(ctx context.Context, r Receive) (PushResult, error) {
	if r.ID == "" || r.Source == nil || len(r.Updates) == 0 {
		return PushResult{}, refusal("request", nil)
	}
	ctx, cancel := s.d.Deadline(ctx, 30*time.Second)
	defer cancel()
	a, err := s.preparePush(ctx, r, false)
	var tag *gitport.Ref
	if err == nil {
		tag, err = releaseTag(a.Runs)
	}
	if ctx.Err() != nil {
		return PushResult{}, &PushRefusal{Problems: []*Refusal{refusal("timeout", nil)}}
	}
	if err != nil {
		return PushResult{}, err
	}
	if s.isClosing() {
		return PushResult{}, refusal("closed", nil)
	}
	if tag != nil {
		if err = deliverReleaseTag(ctx, r.Forge, *tag); err != nil {
			return PushResult{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receives[r.ID] = a
	return cloneResult(a.Result), nil
}

func (s *Submitter) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}
func (s *Submitter) preparePush(ctx context.Context, r Receive, replay bool) (acceptance, error) {
	a := acceptance{BatchAcceptance: stateport.BatchAcceptance{ReceiveID: r.ID}}
	a.Repository, a.Actor = r.Updates[0].Request.Repository, r.Updates[0].Request.Actor.Clone()
	problems := &PushRefusal{}
	for _, u := range r.Updates {
		d, problem := s.decidePush(ctx, r.ID, u)
		if problem != nil {
			problem.Ref = u.Request.Ref.Name
			if err := a.problem(problems, u.Request, problem, replay); err != nil {
				return a, err
			}
			continue
		}
		request := u.Request
		a.Received = append(a.Received, d.Request.Clone())
		request.Actor, request.Trigger, request.Release = d.Request.Actor.Clone(), d.Request.Trigger, d.Release
		request.ReleaseHead = d.ReleaseHead
		a.Actor = request.Actor.Clone()
		out := Outcome{Ref: u.Request.Ref.Name, State: d.Action}
		if d.Action != "run" {
			a.skip(d)
			continue
		}
		run, err := s.admit(ctx, request, r.Source, admitOptions{mode: admissioncontract.ModeSubmit})
		var e *Refusal
		if errors.As(err, &e) && absentOptionalBuild(e, request) {
			out.State, out.File = "absent", e.File
			a.Result.Outcomes = append(a.Result.Outcomes, out)
			continue
		}
		if err != nil {
			if !errors.As(err, &e) {
				e = refusal("unavailable", err)
			}
			e.Ref = out.Ref
			if err := a.problem(problems, request, e, replay); err != nil {
				return a, err
			}
			continue
		}
		run.Request.ReceiveID, run.Request.PreviousSHA = r.ID, u.PreviousSHA
		a.acceptRun(run, out, s.d.PublicURL)
	}
	if len(problems.Problems) > 0 {
		return a, problems
	}
	return a, nil
}

func (s *Submitter) decidePush(ctx context.Context, id string, u RefUpdate) (service.Decision, *Refusal) {
	if err := s.manualPushFile(u); err != nil {
		return service.Decision{}, err
	}
	d, err := s.d.Service.Decide(ctx, service.RefUpdate{ID: id, Repo: u.Request.Repository, Ref: u.Request.Ref.Name, PreviousSHA: u.PreviousSHA, ObjectSHA: u.Request.Ref.ObjectSHA, CommitSHA: u.Request.Ref.CommitSHA, Actor: u.Request.Actor.Clone(), Run: u.Run})
	if err != nil {
		return service.Decision{}, refusal("decision", err)
	}
	return d, nil
}

func (s *Submitter) manualPushFile(u RefUpdate) *Refusal {
	if u.Request.File == "" {
		return nil
	}
	if !u.Run || u.Deleted {
		return refusal("workflow", nil)
	}
	_, _, err := workflowpolicy.Select(s.d.Routes, workflowpolicy.SelectionRequest{
		Repository: u.Request.Repository, Ref: u.Request.Ref.Name, File: u.Request.File, Manual: true,
	})
	if err != nil {
		return workflowSelectionError(err)
	}
	return nil
}

// releaseTag is the push's one admitted release tag, exactly as admitted, or
// nil. A push carries at most one release: several would reach the forge one
// by one, and a refusal of a later one would leave earlier ones without a run.
func releaseTag(runs []stateport.Acceptance) (*gitport.Ref, error) {
	var tag *gitport.Ref
	for _, run := range runs {
		if run.Prepared == nil || !run.Release {
			continue
		}
		if tag != nil {
			e := refusal("release_count", nil)
			e.Ref = run.Request.Ref
			return nil, &PushRefusal{Problems: []*Refusal{e}}
		}
		v := run.Prepared.View()
		tag = &gitport.Ref{Name: v.Ref, ObjectSHA: v.ObjectSHA, CommitSHA: v.CommitSHA}
	}
	return tag, nil
}

// deliverReleaseTag puts the admitted release tag on the forge before any ref
// moves, so no release run exists for a tag the forge lacks. A failed delivery
// refuses the whole push; a delivered tag stays, and its version is then used
// even if the run fails.
func deliverReleaseTag(ctx context.Context, forge TagForge, tag gitport.Ref) error {
	var cause error
	if forge != nil {
		if cause = forge.DeliverTag(ctx, tag); cause == nil {
			return nil
		}
	}
	e := refusal("tag_delivery", cause)
	e.Ref = tag.Name
	return &PushRefusal{Problems: []*Refusal{e}}
}
func (a *acceptance) skip(d service.Decision) {
	if d.Deletion != nil {
		deletion := *d.Deletion
		deletion.Request, deletion.Publication = deletion.Request.Clone(), deletion.Publication.Clone()
		a.Deletions = append(a.Deletions, deletion)
	}
	a.Result.Outcomes = append(a.Result.Outcomes, Outcome{Ref: d.Request.Ref, State: d.Action})
}
func (a *acceptance) problem(problems *PushRefusal, r Request, e *Refusal, replay bool) error {
	if !replay {
		problems.Problems = append(problems.Problems, e)
		return nil
	}
	if !publicerr.Permanent(e) {
		return e
	}
	if r.RunID != "" {
		findings := e.Problems
		if len(findings) == 0 {
			findings = []admissioncontract.Finding{{Code: e.Code, Reason: e.Reason(), Remedy: e.Remedy()}}
		}
		request := ciRequest(r)
		request.ReceiveID = a.ReceiveID
		a.Runs = append(a.Runs, stateport.Acceptance{Request: request, Rejected: &stateport.RejectedAdmission{ID: r.RunID, Findings: findings, Failure: stepmodel.FailureRecord{Class: stepmodel.FailureAuthor, Retry: stepmodel.RetryHint{Action: stepmodel.RetryRerun}}}})
	}
	a.Result.Outcomes = append(a.Result.Outcomes, Outcome{Ref: r.Ref.Name, RunID: r.RunID, State: "failed", File: e.File, Problem: e})
	return nil
}

// Deliver commits under C6's repository lock; only feedback has a ten-second bound.
func (s *Submitter) Deliver(ctx context.Context, r Receive) (PushResult, error) {
	if r.ID == "" {
		return PushResult{}, refusal("request", nil)
	}
	if err := s.begin(); err != nil {
		return PushResult{}, err
	}
	defer s.tasks.Done()
	if len(r.Updates) == 0 {
		work, cancel := s.d.Deadline(ctx, s.d.SourceTimeout)
		defer cancel()
		old, err := s.d.Runs.AcceptedRuns(work, r.ID)
		if err != nil {
			return PushResult{}, err
		}
		return s.feedback(old), nil
	}
	result, err := s.deliver(ctx, r, func(ctx context.Context) (acceptance, error) {
		return s.preparePush(ctx, Receive{ID: r.ID, Updates: r.Updates, Source: s.d.Source}, true)
	})
	result.AssistIgnored = !assistEnabled && slices.ContainsFunc(r.Updates, func(u RefUpdate) bool { return u.Request.Assist })
	return result, err
}
func (s *Submitter) deliver(ctx context.Context, r Receive, prepare func(context.Context) (acceptance, error)) (PushResult, error) {
	work, cancelWork := s.submissionContext(ctx)
	defer cancelWork()
	work, cancelSource := s.d.Deadline(work, s.d.SourceTimeout)
	defer cancelSource()
	old, err := s.d.Runs.AcceptedRuns(work, r.ID)
	if err == nil {
		s.Forget(r.ID)
		return s.feedback(old), nil
	}
	if publicerr.Classify(err).Code() != publicerr.NotFound {
		return PushResult{}, err
	}
	s.mu.Lock()
	a := s.receives[r.ID]
	s.mu.Unlock()
	if a.ReceiveID == "" {
		a, err = prepare(work)
		if err != nil {
			return PushResult{}, err
		}
	}
	saved, err := s.d.Service.Accept(work, a.BatchAcceptance)
	if err != nil {
		return PushResult{}, err
	}
	s.Forget(r.ID)
	if !saved.Fresh {
		return s.feedback(saved.Runs), nil
	}
	done := make(chan PushResult, 1)
	s.tasks.Add(1)
	go func() {
		defer s.tasks.Done()
		background, cancel := s.submissionContext(context.WithoutCancel(ctx))
		defer cancel()
		done <- s.startRuns(background, a, saved.Runs)
	}()
	wait, cancel := s.d.Deadline(ctx, 10*time.Second)
	defer cancel()
	select {
	case c := <-done:
		return c, nil
	case <-wait.Done():
		return cloneResult(a.Result), nil
	}
}
func (s *Submitter) startRuns(ctx context.Context, a acceptance, saved []stateport.Run) PushResult {
	result := cloneResult(a.Result)
	for _, run := range a.Runs {
		if run.Prepared == nil {
			continue
		}
		err := s.start(ctx, saved[slices.IndexFunc(saved, func(r stateport.Run) bool { return r.ID == run.Prepared.View().RunID })], run.Prepared)
		out := &result.Outcomes[slices.IndexFunc(result.Outcomes, func(o Outcome) bool { return o.RunID == run.Prepared.View().RunID })]
		if err == nil {
			out.State = "started"
			continue
		}
		var e *Refusal
		if !errors.As(err, &e) {
			e = refusal("submission", err)
		}
		if e.Code != "submission" {
			out.State = "failed"
		}
		e.Ref = out.Ref
		out.Problem = e
	}
	return result
}
func (s *Submitter) Forget(id string) { s.mu.Lock(); defer s.mu.Unlock(); delete(s.receives, id) }
func cloneResult(r PushResult) PushResult {
	out := PushResult{Outcomes: append([]Outcome(nil), r.Outcomes...), AssistIgnored: r.AssistIgnored, assistNotUsed: slices.Clone(r.assistNotUsed)}
	for i, o := range out.Outcomes {
		if o.Problem != nil {
			e := *o.Problem
			e.Problems = append(e.Problems[:0:0], e.Problems...)
			out.Outcomes[i].Problem = &e
		}
	}
	return out
}
func (r PushResult) Lines() []string {
	var lines []string
	if r.AssistIgnored {
		lines = append(lines, messages()["assist"].remedy)
	}
	if !r.AssistIgnored {
		lines = append(lines, assistRefusalLines(r.assistNotUsed)...)
	}
	for _, o := range r.Outcomes {
		problem := o.Problem
		if problem == nil {
			problem = &Refusal{Code: o.State, Ref: join(o.Ref, o.RunID), File: o.File}
		}
		line := problem.Error()
		if o.WatchURL != "" {
			line += "\n  watch: " + o.WatchURL
		}
		lines = append(lines, line)
	}
	return lines
}
func (s *Submitter) feedback(runs []stateport.Run) PushResult {
	var result PushResult
	for _, r := range runs {
		out := Outcome{Ref: r.Ref, RunID: r.ID, State: "accepted", File: r.Admission.SelectedFile, WatchURL: strings.TrimRight(s.d.PublicURL, "/") + "/runs/" + r.ID}
		if r.Origin.WorkflowUID != "" {
			out.State = "started"
		}
		if r.Status == stepmodel.RunFailed {
			out.State = "failed"
			out.Problem = &Refusal{Code: "failed", Ref: r.Ref, File: out.File, Detail: r.Detail, Problems: r.Findings}
		}
		result.Outcomes = append(result.Outcomes, out)
		result.assistNotUsed = append(result.assistNotUsed, assistRefusals(assistEnabled && r.Origin.AssistRequested, r.Admission.Plan)...)
	}
	return result
}

func (a *acceptance) acceptRun(run stateport.Acceptance, out Outcome, publicURL string) {
	a.Runs = append(a.Runs, run)
	a.Result.assistNotUsed = append(a.Result.assistNotUsed, assistRefusals(assistEnabled && run.Request.Assist, run.Prepared.View().Plan)...)
	out.RunID, out.State = run.Prepared.View().RunID, "accepted"
	out.WatchURL = strings.TrimRight(publicURL, "/") + "/runs/" + out.RunID
	a.Result.Outcomes = append(a.Result.Outcomes, out)
}

func absentOptionalBuild(e *Refusal, r Request) bool {
	return e.Code == "missing" && !r.Release && r.File == ""
}
