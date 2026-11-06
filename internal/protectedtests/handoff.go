package protectedtests

import "errors"

// HandoffExpectation comes from the current parent run and fresh protected
// policy, never from candidate logs or a repository-authored resource payload.
// The durable execution remains the evidence: this value is not a bearer token.
type HandoffExpectation struct {
	RunID        string
	RepoID       int64
	Repository   string
	CandidateSHA string
	BaselineSHA  string
	Contract     *Contract
}

// ValidateHandoff requires a journaled successful protected Workflow AND exact
// UID cleanup before the parent Workflow may receive credentials or publish.
// Callers must load the durable journal and fresh policy again at every gate;
// a prior success cannot authorize a changed run, policy, recipe or repository.
// approve must consult current operator authority, not the stored recipe alone.
// Grant admission, parent cancellation and mutation/audit fences remain the
// caller's responsibility; a generic resource grant is never a trusted verdict.
func (e Execution) ValidateHandoff(expected HandoffExpectation, approve func(Recipe) error) error {
	descriptor, err := e.Intent.Descriptor()
	if err != nil {
		return err
	}
	if err = descriptor.MatchContract(expected.Contract); err != nil {
		return err
	}
	if approve == nil {
		return errors.New("protected handoff requires current recipe approval")
	}
	if err = approve(e.Intent.Recipe); err != nil {
		return err
	}
	if !e.Submitted || !e.Cleaned || e.Rejected || e.Refusal != nil || e.Receipt == nil || e.WorkflowUID == "" || e.WorkflowUID != e.Receipt.WorkflowUID ||
		e.Intent.RunID != expected.RunID || e.Intent.RepoID != expected.RepoID || e.Intent.Repository != expected.Repository ||
		e.Intent.CandidateSHA != expected.CandidateSHA || e.Intent.BaselineSHA != expected.BaselineSHA {
		return errors.New("protected handoff does not match current run and cleanup policy")
	}
	return e.Receipt.Validate(e.Intent)
}
