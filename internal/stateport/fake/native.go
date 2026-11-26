package fake

import (
	"context"
	"reflect"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type nativeEvidence struct {
	Sequence int64
	Expected int
	Nodes    []stateport.Node
	Stored   []stateport.Node
}

func (f *Runs) BindWorkflow(ctx context.Context, a stateport.Action, b nativebinding.RunBinding) error {
	if a.Actor == "" {
		return runProblem(publicerr.InvalidInput)
	}
	if err := b.Validate(); err != nil {
		return err
	}
	return f.transact(ctx, a.Events, func(m *runMemory) error {
		r, ok := m.runs[b.RunID]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		if !r.HasAdmission {
			return runProblem(publicerr.InvalidTransition)
		}
		if !nativeBindingMatches(r, b) {
			return runProblem(publicerr.Conflict)
		}
		if r.Origin.NativeUID() != "" {
			if r.Origin.NativeUID() == b.Workflow.UID && r.Origin.WorkflowName == b.Workflow.Name {
				return noRunChange{}
			}
			return runProblem(publicerr.Conflict)
		}
		if runTerminal(r) {
			return runProblem(publicerr.InvalidTransition)
		}
		r.Origin.WorkflowUID, r.Origin.WorkflowName = b.Workflow.UID, b.Workflow.Name
		r.Revision++
		m.runs[r.ID] = r
		m.record(a.Actor, "run.bind", r.ID, "")
		return nil
	})
}

// SaveProgress retains the native component's single projection without a control revision.
func (f *Runs) SaveProgress(ctx context.Context, id string, p stateport.Progress) error {
	steps := p.Steps
	return f.transact(ctx, nil, func(m *runMemory) error {
		r, ok := m.runs[id]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		if !r.HasAdmission {
			return runProblem(publicerr.InvalidTransition)
		}
		before := r.Clone()
		if !stateport.ValidNativeSteps(r.Admission.Plan, steps) || publicerr.CheckMessage(p.Waiting, 40) != nil {
			return runProblem(publicerr.InvalidInput)
		}
		steps = stateport.PlanSteps(r.Admission.Plan, steps)
		counts, err := stepmodel.CountSteps(steps, stateport.NativeStepsExact(r.Admission.Plan, steps))
		if err != nil {
			return err
		}
		r.LogicalStepCounts = counts
		r.StartedAt = stepmodel.RunStart(r.StartedAt, steps)
		if !runTerminal(r) {
			r.Status, r.DisplayReason = stepmodel.Progress(r.Status, r.DisplayReason, steps, p.Waiting)
		}
		r.Steps = append([]stepmodel.RunStep{}, steps...)
		r = r.Clone()
		m.runs[id] = r
		if reflect.DeepEqual(before.RunSummary, r.RunSummary) {
			return noRunChange{}
		}
		return m.appendEvents([]stateport.EventWrite{stateport.ProgressEvent(r, m.eventID+1, m.now)})
	})
}
func (f *Runs) BeginEvidence(ctx context.Context, a stateport.Action, in stateport.EvidenceStart) (int64, error) {
	if in.Nodes < 1 {
		return 0, runProblem(publicerr.InvalidInput)
	}
	var sequence int64
	err := f.transact(ctx, a.Events, func(m *runMemory) error {
		r, ok := m.runs[in.RunID]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		if !r.HasAdmission {
			return runProblem(publicerr.InvalidTransition)
		}
		sequence = m.native[in.RunID].Sequence + 1
		m.native[in.RunID] = nativeEvidence{Sequence: sequence, Expected: in.Nodes, Stored: m.native[in.RunID].Stored}
		r.EvidenceComplete = false
		m.runs[r.ID] = r
		return nil
	})
	return sequence, err
}
func (f *Runs) PutNodes(ctx context.Context, a stateport.Action, b stateport.NodeBatch) error {
	if len(b.Nodes) < 1 || len(b.Nodes) > 128 {
		return runProblem(publicerr.InvalidInput)
	}
	return f.transact(ctx, a.Events, func(m *runMemory) error {
		r, ok := m.runs[b.RunID]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		e := m.native[b.RunID]
		if e.Sequence == 0 || b.Sequence != e.Sequence || b.Offset != len(e.Nodes) || len(e.Nodes)+len(b.Nodes) > e.Expected {
			return runProblem(publicerr.Conflict)
		}
		if err := validateNativeBatch(r.Admission.Plan, e.Nodes, b.Nodes); err != nil {
			return err
		}
		stored, err := mergeNativeNodes(e.Stored, b.Nodes)
		if err != nil {
			return err
		}
		e.Stored = stored
		e.Nodes = append(cloneNodes(e.Nodes), cloneNodes(b.Nodes)...)
		m.native[b.RunID] = e
		return nil
	})
}
func (f *Runs) CompleteEvidence(ctx context.Context, a stateport.Action, c stateport.EvidenceCompletion) error {
	return f.transact(ctx, a.Events, func(m *runMemory) error {
		r, ok := m.runs[c.RunID]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		e := m.native[c.RunID]
		if e.Sequence == 0 || e.Sequence != c.Sequence || len(e.Nodes) != e.Expected {
			return runProblem(publicerr.Conflict)
		}
		for _, n := range e.Stored {
			if !nativeComplete(n) {
				return runProblem(publicerr.InvalidTransition)
			}
		}
		r.EvidenceComplete = true
		m.runs[r.ID] = r
		return nil
	})
}
func (f *Runs) Nodes(ctx context.Context, id string) ([]stateport.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := f.data.runs[id]; !ok {
		return nil, runProblem(publicerr.NotFound)
	}
	return cloneNodes(f.data.native[id].Stored), nil
}
func nativeComplete(n stateport.Node) bool {
	return n.Credential != nil && (n.Status == stepmodel.StepFailed || n.Status == stepmodel.StepSkipped || n.Status == stepmodel.StepPassed && n.Exit != nil)
}
func cloneNodes(nodes []stateport.Node) []stateport.Node {
	out := append([]stateport.Node(nil), nodes...)
	for i := range out {
		n := &out[i]
		n.Order = append([]int(nil), n.Order...)
		n.Exit = copyNativeValue(n.Exit)
		n.Credential = copyNativeValue(n.Credential)
		n.StartedAt = copyNativeValue(n.StartedAt)
		n.FinishedAt = copyNativeValue(n.FinishedAt)
	}
	return out
}
func copyNativeValue[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func nativeBindingMatches(r stateport.Run, b nativebinding.RunBinding) bool {
	return b.RepositoryID == r.RepositoryID && b.Revision.Repository == r.Repo && b.Revision.Commit == *r.CommitSHA && b.Ref == r.Ref && b.Trigger == string(r.Origin.Trigger) && b.Identity == r.Admission.Digest && b.SpecSHA256 == r.Admission.SpecDigest
}

func validateNativeBatch(plan admissioncontract.Plan, previous, batch []stateport.Node) error {
	ids := map[string]bool{}
	for _, n := range previous {
		ids[n.NodeID] = true
	}
	for _, n := range batch {
		if ids[n.NodeID] || n.NodeID == "" || n.Attempt < 1 || !n.Status.Valid() || !stateport.NativePlanPath(plan, n.Path) {
			return runProblem(publicerr.InvalidInput)
		}
		ids[n.NodeID] = true
	}
	return nil
}
