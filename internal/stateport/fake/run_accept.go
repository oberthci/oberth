package fake

import (
	"context"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (f *Runs) AcceptBatch(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	if b.ReceiveID == "" || len(b.ReceiveID) > 512 || b.Actor.Identity == "" || b.Actor.AuthMethod == "" {
		return stateport.AcceptanceResult{}, runProblem(publicerr.InvalidInput)
	}
	events := append([]stateport.EventWrite(nil), b.Events...)
	for i := range b.Runs {
		events = append(events, b.Runs[i].Events...)
	}
	var out []stateport.Run
	fresh := false
	err := f.movingChange(ctx, "AcceptBatch", events, func(m *runMemory) error {
		binding := receiptBinding(b)
		if old, ok := m.receipts[b.ReceiveID]; ok {
			if old.Binding != binding {
				return runProblem(publicerr.Conflict)
			}
			out = make([]stateport.Run, 0, len(old.IDs))
			for _, id := range old.IDs {
				r := m.runs[id]

				out = append(out, r.Clone())
			}
			return noRunChange{}
		}
		var err error
		out, err = m.writeReceive(b, f.clock())
		if err != nil {
			return err
		}
		fresh = true
		return nil
	})
	if err != nil {
		return stateport.AcceptanceResult{}, err
	}
	return stateport.AcceptanceResult{Runs: out, Fresh: fresh}, nil
}

func receiptBinding(b stateport.BatchAcceptance) string {
	type accepted struct {
		Request     model.CIRequest
		ID, Digest  string
		Rejected    *stateport.RejectedAdmission
		Release     bool
		RerunOf     string
		PromotionID string
		Actions     []model.AuditAction
	}
	rows := []accepted{}
	for _, a := range b.Runs {
		r := a.Request.Clone()
		if r.ReceiveID == "" {
			r.ReceiveID = b.ReceiveID
		}
		id, digest := "", ""
		if a.Prepared != nil {
			v := a.Prepared.View()
			id, digest = v.RunID, v.Digest
		}
		rows = append(rows, accepted{r, id, digest, a.Rejected, a.Release, a.RerunOf, a.PromotionID, a.Actions})
	}
	return jsonValue(struct {
		Repository string
		Actor      any
		Runs       []accepted
		Deletions  []stateport.BranchDeletion
	}{b.Repository, b.Actor, rows, b.Deletions})
}

func (m *runMemory) acceptRun(b stateport.BatchAcceptance, a stateport.Acceptance, at time.Time) (stateport.Run, error) {
	if err := m.rerun(a); err != nil {
		return stateport.Run{}, err
	}
	r := a.Request
	if r.RequestedFile != "" && sourcecontract.ValidateFilePath(r.RequestedFile) != nil {
		return stateport.Run{}, runProblem(publicerr.InvalidInput)
	}
	if r.ReceiveID == "" {
		r.ReceiveID = b.ReceiveID
	}
	if r.ReceiveID != b.ReceiveID || r.Repo != b.Repository || jsonValue(r.Actor) != jsonValue(b.Actor) {
		return stateport.Run{}, runProblem(publicerr.InvalidInput)
	}
	result := stateport.Run{RunSummary: stepmodel.RunSummary{Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA, Status: stepmodel.RunQueued, CreatedAt: at.UTC()}, Origin: model.RunOrigin{RunCause: model.RunCause{Actor: r.Actor.Identity, Trigger: r.Trigger, ScheduleEntry: r.ScheduleEntry, AssistRequested: r.Assist, RerunOf: a.RerunOf}, RequestedFile: r.RequestedFile}, Revision: 1, Release: a.Release, RepositoryID: m.repos[r.Repo].ID}
	if err := applyFakeAdmission(&result, a); err != nil {
		return result, err
	}
	if _, ok := m.runs[result.ID]; ok {
		return result, runProblem(publicerr.Conflict)
	}
	if err := m.linkPromotion(a, result); err != nil {
		return result, err
	}
	m.sequence++
	result.Sequence = m.sequence
	m.record(r.Actor.Identity, "run.accept", result.ID, "")
	if err := m.recordAcceptActions(a.Actions); err != nil {
		return result, err
	}
	return result, nil
}

func applyFakeAdmission(r *stateport.Run, a stateport.Acceptance) error {
	if a.Prepared != nil {
		v := a.Prepared.View()
		if err := fakePrepared(a, v); err != nil {
			return err
		}
		r.ID = v.RunID
		r.Admission = v
		r.HasAdmission = true
		r.Total = len(v.Plan.Entries)
		r.Exact = v.Plan.Exact
		r.Pending = r.Total
		return nil
	}
	if a.Child != nil {
		return runProblem(publicerr.InvalidInput)
	}
	if err := fakeRejected(a.Rejected); err != nil {
		return err
	}
	r.ID = a.Rejected.ID
	r.Status = stepmodel.RunFailed
	r.DisplayReason = stateport.ReasonAdmission
	r.FinishedAt = new(r.CreatedAt)
	r.Findings = append([]admissioncontract.Finding(nil), a.Rejected.Findings...)
	failure := a.Rejected.Failure.Clone()
	r.Failure = &failure
	return nil
}

func (f *Runs) AcceptedRuns(ctx context.Context, id string) ([]stateport.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "AcceptedRuns", false); err != nil {
		return nil, err
	}
	receipt, ok := f.data.receipts[id]
	if !ok {
		return nil, runProblem(publicerr.NotFound)
	}
	rows := make([]stateport.Run, 0, len(receipt.IDs))
	for _, id := range receipt.IDs {
		r := f.data.runs[id]

		rows = append(rows, r.Clone())
	}
	return rows, nil
}

func (m *runMemory) writeReceive(b stateport.BatchAcceptance, at time.Time) ([]stateport.Run, error) {
	if _, ok := m.repos[b.Repository]; !ok {
		return nil, runProblem(publicerr.NotFound)
	}
	for _, d := range b.Deletions {
		if err := m.deleteBranch(b, d, at); err != nil {
			return nil, err
		}
	}
	out := make([]stateport.Run, 0, len(b.Runs))
	ids := []string{}
	for _, a := range b.Runs {
		r, err := m.acceptRun(b, a, at)
		if err != nil {
			return nil, err
		}
		m.runs[r.ID] = r
		out = append(out, r.Clone())
		ids = append(ids, r.ID)
	}
	if len(b.Runs) == 0 && len(b.Deletions) == 0 {
		m.record(b.Actor.Identity, "receive.accept", b.ReceiveID, "")
	}
	m.receipts[b.ReceiveID] = runReceipt{receiptBinding(b), ids}
	return out, nil
}

func fakePrepared(a stateport.Acceptance, v admissioncontract.PreparedView) error {
	if a.Rejected != nil || v.RunID == "" || v.Digest == "" || a.Request.CommitSHA == nil {
		return runProblem(publicerr.InvalidInput)
	}
	if err := fakePreparedBinding(a.Request, v); err != nil {
		return err
	}
	return fakePlan(v.Plan)
}

func fakePreparedBinding(r model.CIRequest, v admissioncontract.PreparedView) error {
	if v.Trigger != r.Trigger || v.ScheduleEntry != r.ScheduleEntry || v.Repository != r.Repo || v.Ref != r.Ref || v.ObjectSHA != r.ObjectSHA || v.CommitSHA != *r.CommitSHA {
		return runProblem(publicerr.InvalidInput)
	}
	if r.RequestedFile != "" && r.RequestedFile != v.SelectedFile {
		return runProblem(publicerr.InvalidInput)
	}
	return nil
}
func fakePlan(plan admissioncontract.Plan) error {
	if len(plan.Entries) > admissioncontract.MaxPlanEntries || len(jsonValue(plan)) > admissioncontract.MaxPlanBytes {
		return runProblem(publicerr.InvalidInput)
	}
	seen := map[string]bool{}
	for _, p := range plan.Entries {
		if seen[p.Path] {
			return runProblem(publicerr.InvalidInput)
		}
		seen[p.Path] = true
	}
	return nil
}
func fakeRejected(r *stateport.RejectedAdmission) error {
	if r == nil || r.ID == "" || len(r.Findings) == 0 {
		return runProblem(publicerr.InvalidInput)
	}
	if !r.Failure.Class.Valid() || !r.Failure.Retry.Action.Valid() {
		return runProblem(publicerr.InvalidInput)
	}
	for _, finding := range r.Findings {
		if finding.Code == "" || publicerr.CheckMessage(finding.Reason, 80) != nil || publicerr.CheckMessage(finding.Remedy, 120) != nil {
			return runProblem(publicerr.InvalidInput)
		}
	}
	return nil
}

func (m *runMemory) recordAcceptActions(actions []model.AuditAction) error {
	for _, action := range actions {
		if action.Actor == "" || action.Action == "" || action.ResourceType == "" {
			return runProblem(publicerr.InvalidInput)
		}
		action.ID = int64(len(m.audits) + 1)
		m.audits = append(m.audits, action)
	}
	return nil
}
