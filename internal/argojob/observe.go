package argojob

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"reflect"
	"slices"

	"time"
)

// Runs is the durable owner's binding and recovery surface.
type Runs interface {
	Run(context.Context, string) (stateport.Run, error)
	Runs(context.Context, stateport.RunFilter) ([]stateport.Run, error)
	Steps(context.Context, string) ([]stepmodel.RunStep, error)
	BindWorkflow(context.Context, stateport.Action, nativebinding.RunBinding) error
	Obligations(context.Context, string) ([]stateport.Obligation, error)
}

// ProgressStore retains the native projection and raw evidence without control revisions.
type ProgressStore interface {
	SaveProgress(context.Context, string, stateport.Progress) error
	AdvanceNative(context.Context, string) error
	Nodes(context.Context, string) ([]stateport.Node, error)
	BeginEvidence(context.Context, stateport.Action, stateport.EvidenceStart) (int64, error)
	PutNodes(context.Context, stateport.Action, stateport.NodeBatch) error
	CompleteEvidence(context.Context, stateport.Action, stateport.EvidenceCompletion) error
}

// Reporter commits the terminal fact and best-effort informational hold notices.
type Reporter interface {
	Finished(context.Context, string, Observation) error
	Held(context.Context, model.HeldNotice) error
}

const observationAttempts = 5

type tracking struct {
	child                    *childObservation
	binding                  nativebinding.RunBinding
	plan                     admissioncontract.Plan
	previous                 []stepmodel.RunStep
	previousNodes            []stateport.Node
	waiting                  string
	completed, lost, settled bool
	final                    *Observation
	refusals                 int
	lastError                string
	evidenceUntil            time.Time
	captures                 map[string]bool
	pending                  map[string]captureJob
	holds                    map[string]bool
}

type trackingSeed struct {
	child *childObservation
	steps []stepmodel.RunStep
	nodes []stateport.Node
}

func (e *Executor) track(b nativebinding.RunBinding, plan admissioncontract.Plan, seed trackingSeed) {
	e.mu.Lock()
	delete(e.recoveryErrors, b.Workflow.Name)
	if t, ok := e.tracked[b.Workflow.Name]; !ok || t.binding != b {
		t = &tracking{binding: b, plan: plan.Clone(), previous: (stepmodel.RunSummary{Steps: seed.steps}).Clone().Steps, previousNodes: seed.nodes, captures: map[string]bool{}, holds: map[string]bool{}}
		t.child = seed.child
		if seed.child != nil {
			t.final = seed.child.refused
		}
		e.tracked[b.Workflow.Name] = t
	}
	e.mu.Unlock()
	e.queue.Add(b.Workflow.Name)
}
func (e *Executor) Untrack(name string) {
	e.mu.Lock()
	delete(e.tracked, name)
	delete(e.recoveryErrors, name)
	e.mu.Unlock()
}

// Run consumes recovery and native change notifications through the same queue.
func (e *Executor) Run(ctx context.Context) error {
	e.queue.Add("")
	logsDone := e.startCaptures(ctx)
	defer func() { e.queue.ShutDown(); <-logsDone }()
	go func() { <-ctx.Done(); e.queue.ShutDown() }()
	for {
		name, closed := e.queue.Get()
		if closed {
			return ctx.Err()
		}
		if name == "" {
			if err := e.recover(ctx); err != nil {
				e.logError(ctx, "recovery", err)
				e.queue.AddRateLimited(name)
			} else {
				e.queue.Forget(name)
			}
		} else {
			e.process(ctx, name)
		}
		e.queue.Done(name)
	}
}
func (e *Executor) recover(ctx context.Context) error {
	if e.config.Runs == nil {
		return nil
	}
	var failures []error
	for _, status := range []stepmodel.RunStatus{stepmodel.RunQueued, stepmodel.RunRunning} {
		failures = append(failures, e.recoverStatus(ctx, status))
	}
	return errors.Join(failures...)
}

func (e *Executor) recoverStatus(ctx context.Context, status stepmodel.RunStatus) error {
	q := stateport.RunFilter{Status: status, Limit: 200}
	var failures []error
	for {
		page, cancel := context.WithTimeout(ctx, e.config.ObservationTimeout)
		rows, err := e.config.Runs.Runs(page, q)
		cancel()
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		for _, r := range rows {
			if r.Origin.WorkflowUID != "" {
				if err = e.restore(ctx, r); err != nil {
					failures = append(failures, err)
					e.mu.Lock()
					old := e.recoveryErrors[r.Origin.WorkflowName]
					e.recoveryErrors[r.Origin.WorkflowName] = err.Error()
					e.mu.Unlock()
					if old != err.Error() {
						e.logError(ctx, r.ID, err)
					}
				}
			}
		}
		if len(rows) < q.Limit {
			return errors.Join(failures...)
		}
		q.Cursor = rows[len(rows)-1].ID
	}
}
func (e *Executor) restore(ctx context.Context, r stateport.Run) error {
	ctx, cancel := context.WithTimeout(ctx, e.config.ObservationTimeout)
	defer cancel()
	var err error
	r, err = e.config.Runs.Run(ctx, r.ID)
	if err != nil {
		return err
	}
	if scheduledChildRun(r) && terminalChild(r) {
		return nil
	}
	var child *childObservation
	if scheduledChildRun(r) {
		if e.config.ChildRuns == nil {
			return ownership(nil)
		}
		proof, err := e.config.ChildRuns.ChildProof(ctx, r.ID)
		if err != nil {
			return err
		}
		child, err = boundChild(r, proof)
		if err != nil {
			return err
		}
	}
	return e.restoreSnapshot(ctx, r, child)
}

func (e *Executor) restoreSnapshot(ctx context.Context, r stateport.Run, child *childObservation) error {
	steps, err := e.config.Runs.Steps(ctx, r.ID)
	if err != nil {
		return err
	}
	seed := trackingSeed{steps: steps, child: child}
	if e.config.Progress != nil {
		seed.nodes, err = e.config.Progress.Nodes(ctx, r.ID)
		if err != nil {
			return err
		}

	}
	b := runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: r.Admission.Identity.Namespace, Name: r.Origin.WorkflowName, UID: r.Origin.WorkflowUID})
	e.track(b, r.Admission.Plan, seed)
	return nil
}
func (e *Executor) process(ctx context.Context, name string) {
	e.mu.Lock()
	t := e.tracked[name]
	e.mu.Unlock()
	if t == nil {
		e.discoverChild(ctx, name)
		return
	}
	parent := ctx
	budget := e.config.ObservationTimeout
	if t.completed {
		budget = min(budget, t.evidenceUntil.Sub(e.config.Now()))
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if e.expired(t) {
		e.exhausted(parent, t, errors.New("terminal evidence window expired"))
		e.queue.Forget(name)
		e.Untrack(name)
		return
	}
	err := e.observe(ctx, t)
	if parent.Err() != nil {
		return
	}
	e.logObservation(ctx, t, err)
	if err == nil {
		t.refusals = 0
		e.queue.Forget(name)
		if t.settled || t.lost {
			e.Untrack(name)
		}
		return
	}
	var readError *nativeReadError
	if t.completed && !errors.As(err, &readError) {
		t.refusals++
	}
	if t.refusals < observationAttempts {
		e.queue.AddRateLimited(name)
		return
	}
	e.exhausted(parent, t, err)
	e.queue.Forget(name)
	e.Untrack(name)
}
func (e *Executor) observe(ctx context.Context, t *tracking) error {
	if t.final != nil && !t.completed {
		if err := e.publish(ctx, t, *t.final); err != nil {
			return err
		}
	}
	if t.lost {
		return e.reportDeleted(ctx, t)
	}
	w, err := e.Adopt(ctx, t.binding)
	if errors.As(err, new(ErrAbsent)) {
		t.lost = true
		return e.reportDeleted(ctx, t)
	}
	if err != nil {
		var foreign *nativebinding.Refusal
		if errors.As(err, &foreign) {
			t.lost = true
			return e.publish(ctx, t, observationFailure(err))
		}
		return &nativeReadError{err}
	}
	pods := e.config.Cache.Pods(w.Name)
	o, err := Project(ctx, Projection{Plan: t.plan, Workflow: w, Pods: pods, Previous: t.previous, PreviousNodes: t.previousNodes})
	if err != nil {
		return &nativeReadError{err}
	}
	var terminalError error
	if (t.final != nil || terminalWorkflow(w)) && !t.completed {
		// A reader told the run finished sees its final steps and counts.
		terminalError = errors.Join(e.saveProgress(ctx, t, o), e.publish(ctx, t, o))
	}
	w = o.workflow
	e.capture(t, w, pods)
	settled := e.evidenceSettled(t, o, pods)
	progressError := e.saveObservation(ctx, t, o, terminalWorkflow(w))
	e.reportHolds(ctx, t, w, pods)
	t.settled = settled
	return errors.Join(terminalError, progressError)
}
func (e *Executor) reportDeleted(ctx context.Context, t *tracking) error {
	return e.publish(ctx, t, Observation{Status: stepmodel.RunFailed, Phase: stepmodel.NativeError, Reason: reasonDeleted, Failure: &stepmodel.FailureRecord{Class: stepmodel.FailureUnknown, Retry: stepmodel.RetryHint{Action: stepmodel.RetryNone}}})
}
func (e *Executor) logError(ctx context.Context, id string, err error) {
	if err == nil {
		e.config.Logger.InfoContext(ctx, "native observation recovered", "run_id", id)
		return
	}
	e.config.Logger.ErrorContext(ctx, reasonObserve, "run_id", id, "cause", err)
}
func (e *Executor) writeEvidence(ctx context.Context, id string, o Observation) error {
	if len(o.Nodes) == 0 {
		return nil
	}
	sequence, err := e.config.Progress.BeginEvidence(ctx, stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: len(o.Nodes)})
	if err != nil {
		return err
	}
	for offset := 0; offset < len(o.Nodes); offset += 128 {
		batch := stateport.NodeBatch{RunID: id, Sequence: sequence, Offset: offset, Nodes: o.Nodes[offset:min(offset+128, len(o.Nodes))]}
		if err = e.config.Progress.PutNodes(ctx, stateport.Action{}, batch); err != nil {
			return err
		}
	}
	if o.Complete {
		return e.config.Progress.CompleteEvidence(ctx, stateport.Action{}, stateport.EvidenceCompletion{RunID: id, Sequence: sequence})
	}
	return nil
}

// saveProgress writes the steps and, for a queued run, what it waits for,
// whenever either changed since the last write.
func (e *Executor) saveProgress(ctx context.Context, t *tracking, o Observation) error {
	p := stateport.Progress{Steps: o.Steps}
	if o.Status == stepmodel.RunQueued {
		p.Waiting = o.Reason
	}
	if reflect.DeepEqual(p.Steps, t.previous) && p.Waiting == t.waiting {
		return nil
	}
	if err := e.config.Progress.SaveProgress(ctx, t.binding.RunID, p); err != nil {
		return err
	}
	t.previous, t.waiting = (stepmodel.RunSummary{Steps: p.Steps}).Clone().Steps, p.Waiting
	return nil
}
func (e *Executor) saveObservation(ctx context.Context, t *tracking, o Observation, terminal bool) error {
	if err := e.saveProgress(ctx, t, o); err != nil {
		return err
	}
	if !reflect.DeepEqual(o.Nodes, t.previousNodes) || terminal {
		if err := e.writeEvidence(ctx, t.binding.RunID, o); err != nil {
			return err
		}
		t.previousNodes = o.Nodes
	}
	return nil
}

func (e *Executor) publish(ctx context.Context, t *tracking, o Observation) error {
	if t.completed {
		return nil
	}
	if t.final == nil {
		o.workflow = nil
		t.final = &o
	}
	if slices.ContainsFunc(t.plan.Entries, func(e admissioncontract.PlanEntry) bool { return e.HoldArmed }) {
		// Finish must see the frozen native results, even on the first observation.
		// Evidence completion remains independent of terminal acknowledgement.
		final := *t.final
		final.Complete = false
		if err := e.writeEvidence(ctx, t.binding.RunID, final); err != nil {
			return err
		}
	}
	err := e.config.Reporter.Finished(ctx, t.binding.RunID, *t.final)
	if err == nil {
		e.mu.Lock()
		t.completed = true
		t.evidenceUntil = e.config.Now().Add(terminalEvidenceGrace)
		e.mu.Unlock()
		e.queue.AddAfter(t.binding.Workflow.Name, terminalEvidenceGrace)
	}
	return err
}
func (e *Executor) exhausted(ctx context.Context, t *tracking, cause error) {
	ctx, cancel := context.WithTimeout(ctx, e.config.ObservationTimeout)
	defer cancel()
	e.config.Metrics.IncompleteEvidence.Add(1)
	e.logError(ctx, t.binding.RunID, errors.Join(cause, e.config.Progress.AdvanceNative(ctx, t.binding.RunID)))
}

// Native reads may fail while Argo continues; they never consume store refusals.
type nativeReadError struct{ error }

func observationFailure(cause error) Observation {
	return Observation{Status: stepmodel.RunFailed, Phase: stepmodel.NativeError, Reason: reasonObserve, Detail: cause.Error(), Failure: &stepmodel.FailureRecord{Class: stepmodel.FailureUnknown, Retry: stepmodel.RetryHint{Action: stepmodel.RetryNone}}}
}

func (e *Executor) logObservation(ctx context.Context, t *tracking, err error) {
	if !t.completed || err == nil {
		text := ""
		if err != nil {
			text = err.Error()
		}
		if text != t.lastError {
			t.lastError = text
			e.logError(ctx, t.binding.RunID, err)
		}
	}
}
