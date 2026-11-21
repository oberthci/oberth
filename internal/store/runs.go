package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func acceptance(input stateport.Acceptance) (acceptedBinding, admissioncontract.PreparedView, error) {
	var view admissioncontract.PreparedView
	if input.Prepared == nil {
		if err := validateRejected(input); err != nil {
			return acceptedBinding{}, view, err
		}
	} else {
		if input.Rejected != nil {
			return acceptedBinding{}, view, refuse(invalid, nil)
		}
		view = input.Prepared.View()
		if err := validateAcceptance(input.Request, view); err != nil {
			return acceptedBinding{}, view, err
		}
		if err := validatePlan(view.Plan); err != nil {
			return acceptedBinding{}, view, err
		}
	}
	return bindingFor(input, view), view, nil
}

func validatePlan(plan admissioncontract.Plan) error {
	if len(plan.Entries) > admissioncontract.MaxPlanEntries || len(encode(plan)) > admissioncontract.MaxPlanBytes {
		return refuse(invalid, nil)
	}
	seen := make(map[string]bool, len(plan.Entries))
	for _, entry := range plan.Entries {
		if seen[entry.Path] {
			return refuse(invalid, nil)
		}
		seen[entry.Path] = true
	}
	return nil
}

func (s *Store) insertRun(ctx context.Context, tx *sql.Tx, input stateport.Acceptance) (stateport.Run, error) {
	r := input.Request
	if input.RerunOf != "" {
		source, err := loadRun(ctx, tx, input.RerunOf)
		if err != nil {
			return stateport.Run{}, err
		}
		if source.Repo != r.Repo || source.Ref != r.Ref {
			return stateport.Run{}, refuse(conflict, nil)
		}
	}
	repoID, err := repositoryNumber(ctx, tx, r.Repo)
	if err != nil {
		return stateport.Run{}, err
	}
	result := s.admittedRun(input, repoID)
	var admission any
	if input.Prepared != nil {
		body, err := json.Marshal(input.Prepared)
		if err != nil {
			return stateport.Run{}, refuse(invalid, err)
		}
		admission = body
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO runs(id,repo_id,ref,status,release,origin,summary,admission,step_count,steps_planned_exact,
steps_pending,findings)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, result.ID, repoID, r.Ref, result.Status, input.Release, encode(result.Origin),
		encode(result.RunSummary), admission, result.Total, result.Exact, result.Pending, encode(result.Findings))
	if err != nil {
		return stateport.Run{}, refuse(conflict, err)
	}
	if err := persistChild(ctx, tx, input, repoID); err != nil {
		return stateport.Run{}, err
	}
	result.Sequence, err = inserted.LastInsertId()
	return result, databaseError(err)
}

func repositoryNumber(ctx context.Context, tx *sql.Tx, canonical string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM repositories WHERE canonical=?`, canonical).Scan(&id)
	return id, databaseError(err)
}

func loadRun(ctx context.Context, tx *sql.Tx, id string) (stateport.Run, error) {
	var r stateport.Run
	var summary, origin, findings string
	var admission []byte
	err := tx.QueryRowContext(ctx, `SELECT repo_id,sequence,revision,summary,origin,admission,evidence_complete,release,step_count,
steps_planned_exact,steps_pending,steps_queued,steps_running,steps_passed,steps_failed,steps_skipped,
steps_unknown,findings
FROM runs WHERE id=?`, id).Scan(&r.RepositoryID, &r.Sequence, &r.Revision, &summary, &origin, &admission, &r.EvidenceComplete,
		&r.Release, &r.Total, &r.Exact, &r.Pending, &r.Queued, &r.Running, &r.Passed, &r.Failed, &r.Skipped, &r.Unknown, &findings)
	if err != nil {
		return r, databaseError(err)
	}
	counts := r.LogicalStepCounts
	r.HasAdmission = len(admission) > 0
	if !r.HasAdmission {
		admission = []byte(`{}`)
	}
	for _, pair := range []struct {
		body   []byte
		target any
	}{{[]byte(summary), &r.RunSummary}, {[]byte(origin), &r.Origin}, {admission, &r.Admission}, {[]byte(findings), &r.Findings}} {
		if err := json.Unmarshal(pair.body, pair.target); err != nil {
			return r, refuse(corrupt, err)
		}
	}
	r.LogicalStepCounts = counts
	return r, nil
}

// Run returns a consistent admission/evidence snapshot for one exact identity.
func (s *Store) Run(ctx context.Context, id string) (stateport.Run, error) {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return stateport.Run{}, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	return loadRun(ctx, tx, id)
}

const timestampFormat = "2006-01-02T15:04:05.000000000Z07:00"

func terminal(status stepmodel.RunStatus) bool {
	return status == stepmodel.RunPassed || status == stepmodel.RunFailed || status == stepmodel.RunCancelled
}
func validRef(ref string) bool {
	return sourcecontract.ValidatePublicRef(ref) == nil
}

func validateAcceptance(r model.CIRequest, view admissioncontract.PreparedView) error {
	if r.CommitSHA == nil {
		return refuse(invalid, nil)
	}
	if err := validateRequest(r); err != nil {
		return err
	}
	return validatePreparedBinding(r, view)
}
func validatePreparedBinding(r model.CIRequest, view admissioncontract.PreparedView) error {
	if view.Digest == "" || view.RunID == "" || view.Repository != r.Repo || view.Ref != r.Ref || view.ObjectSHA != r.ObjectSHA ||
		view.CommitSHA != *r.CommitSHA || view.Trigger != r.Trigger || view.ScheduleEntry != r.ScheduleEntry {
		return refuse(invalid, nil)
	}
	return nil
}

// admittedRun constructs the immutable acceptance projection from the sealed input.
func (s *Store) admittedRun(input stateport.Acceptance, repoID int64) stateport.Run {
	r := input.Request
	result := stateport.Run{
		RepositoryID: repoID,
		Release:      input.Release,
		RunSummary: stepmodel.RunSummary{

			Repo:      r.Repo,
			Ref:       r.Ref,
			ObjectSHA: r.ObjectSHA,
			CommitSHA: r.CommitSHA,
			Status:    stepmodel.RunRunning,
			CreatedAt: s.options.Clock().UTC(),
		},
		Origin: model.RunOrigin{RunCause: model.RunCause{
			Actor:         r.Actor.Identity,
			Trigger:       r.Trigger,
			ScheduleEntry: r.ScheduleEntry,
			RerunOf:       input.RerunOf,
		}, AssistRequested: r.Assist},
		Revision: 1,
	}
	applyAdmission(&result, input)
	if input.Child != nil {
		result.Origin.WorkflowName = input.Child.Run.Workflow.Name
		result.Origin.WorkflowUID = input.Child.Run.Workflow.UID
	}
	return result
}
