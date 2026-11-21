package fake

import (
	"context"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (m *runMemory) publication(a stateport.Action, p model.Publication, r stateport.Run) error {
	if a.Actor == "" || p.Actor != a.Actor || p.ID == "" || p.RunID != r.ID || p.ObjectSHA == "" {
		return runProblem(publicerr.InvalidInput)
	}
	if p.CommitSHA == nil {
		p.CommitSHA = r.CommitSHA
	}
	if !passingPublication(p, r) {
		return runProblem(publicerr.Forbidden)
	}
	if invalidTagBase(p) {
		return runProblem(publicerr.InvalidInput)
	}
	if _, exists := m.publications[p.ID]; exists {
		return runProblem(publicerr.Conflict)
	}
	p.Status = model.PublicationPending
	m.publications[p.ID] = p.Clone()
	m.record(a.Actor, "publication.prepare", p.ID, a.Reason)
	return nil
}

func (m *runMemory) deleteBranch(b stateport.BatchAcceptance, d stateport.BranchDeletion, at time.Time) error {
	if err := fakeDeletion(b, d); err != nil {
		return err
	}
	r, p := d.Request, d.Publication
	if _, exists := m.publications[p.ID]; exists {
		return runProblem(publicerr.Conflict)
	}
	m.cancelBranch(r, at)
	p.Status = model.PublicationPending
	m.publications[p.ID] = p.Clone()
	m.record(r.Actor.Identity, "branch.delete", r.Repo, "")
	m.record(r.Actor.Identity, "publication.prepare", p.ID, "")
	return nil
}

func (f *Runs) Publication(ctx context.Context, id string) (model.Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "Publication", false); err != nil {
		return model.Publication{}, err
	}
	p, ok := f.data.publications[id]
	if !ok {
		return p, runProblem(publicerr.NotFound)
	}
	return p.Clone(), nil
}

func fakeDeletion(b stateport.BatchAcceptance, d stateport.BranchDeletion) error {
	r, p := d.Request, d.Publication
	if r.ReceiveID == "" {
		r.ReceiveID = b.ReceiveID
	}
	if r.ReceiveID != b.ReceiveID || r.Repo != b.Repository || jsonValue(r.Actor) != jsonValue(b.Actor) {
		return runProblem(publicerr.InvalidInput)
	}
	if p.ID == "" || p.Actor != r.Actor.Identity || p.Repo != r.Repo || p.Ref != r.Ref || p.PreviousSHA == nil {
		return runProblem(publicerr.InvalidInput)
	}
	return runlessPublication(p, r.PreviousSHA)
}
func runlessPublication(p model.Publication, previous string) error {
	if p.RunID != "" || p.ObjectSHA != "" || p.CommitSHA != nil || p.PromotionID != "" || *p.PreviousSHA != previous {
		return runProblem(publicerr.InvalidInput)
	}
	return nil
}
func (m *runMemory) cancelBranch(r model.CIRequest, at time.Time) {
	for id, run := range m.runs {
		if run.Repo != r.Repo || run.Ref != r.Ref || runTerminal(run) {
			continue
		}
		run = run.Clone()
		run.Status = stepmodel.RunCancelled
		finishRemainders(&run)
		run.DisplayReason = stateport.ReasonBranchDeleted
		run.Revision++
		run.FinishedAt = new(at.UTC())
		m.runs[id] = run
		key := id + "\x00stop"
		o := m.obligations[key]
		o.RunID = id
		o.Kind = "stop"
		o.Actor = r.Actor.Identity
		o.Revision++
		if o.Value != "Terminate" {
			o.Value = stateport.ObligationStop
		}
		m.obligations[key] = o
	}
}

func passingPublication(p model.Publication, r stateport.Run) bool {
	return r.Status == stepmodel.RunPassed && !r.Origin.HasAssistance() && p.ObjectSHA == r.ObjectSHA && p.Repo == r.Repo && r.CommitSHA != nil && p.CommitSHA != nil && *p.CommitSHA == *r.CommitSHA
}
func invalidTagBase(p model.Publication) bool {
	return strings.HasPrefix(p.Ref, "refs/tags/") && p.PreviousSHA != nil && *p.PreviousSHA != ""
}
