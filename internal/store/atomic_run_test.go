package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func runBinding(r stateport.Run) nativebinding.RunBinding {
	return nativebinding.RunBinding{RunID: r.ID, RepositoryID: r.RepositoryID, Revision: sourcecontract.Revision{Repository: r.Repo, Commit: *r.CommitSHA}, Ref: r.Ref, Trigger: string(r.Origin.Trigger), Identity: r.Admission.Digest, SpecSHA256: r.Admission.SpecDigest, Workflow: nativebinding.Object{Namespace: "runs", Name: "workflow", UID: "uid"}}
}

func reacceptance(t *testing.T, input stateport.Acceptance, id string) stateport.Acceptance {
	t.Helper()
	view := input.Prepared.View()
	view.RunID = id
	var err error
	input.Prepared, err = admissioncontract.Seal(view)
	requireOK(t, err)
	input.Request.ReceiveID = "receive-" + id
	return input
}

func TestAtomicRunFacts(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{
		{"run.accept", func(t *testing.T, s *Store) func(stateport.Action) error {
			_, input := accepted(t, s, "source")
			input = reacceptance(t, input, "next")
			return func(a stateport.Action) error {
				input.Events = a.Events
				_, err := acceptForTest(t.Context(), s, input)
				return err
			}
		}},
		{"run.bind", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			return func(a stateport.Action) error { return s.BindWorkflow(t.Context(), a, runBinding(r)) }
		}},
		{"run.finish", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			return func(a stateport.Action) error {
				return s.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed})
			}
		}},
		{"run.delete_branch", func(t *testing.T, s *Store) func(stateport.Action) error {
			_, input := accepted(t, s, "run")
			input.Request.ReceiveID = "delete"
			return func(a stateport.Action) error {
				return recordDeletion(t, s, deletionForTest(input.Request), a.Events...)
			}
		}},
	})
}

func completeNode(s *Store) stateport.Node {
	return stateport.Node{NodeID: "node", Path: "build", Attempt: 1, Status: stepmodel.StepFailed, Credential: new(false), Exit: new(1), StartedAt: new(s.options.Clock()), FinishedAt: new(s.options.Clock())}
}

func TestAtomicObservationFacts(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{
		{"evidence.begin", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			return func(a stateport.Action) error {
				_, err := s.BeginEvidence(t.Context(), a, stateport.EvidenceStart{RunID: r.ID, Nodes: 129})
				return err
			}
		}},

		{"nodes.put", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			sequence, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: r.ID, Nodes: 1})
			requireOK(t, err)
			return func(a stateport.Action) error {
				return s.PutNodes(t.Context(), a, stateport.NodeBatch{RunID: r.ID, Sequence: sequence, Nodes: []stateport.Node{completeNode(s)}})
			}
		}},
		{"evidence.complete", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			requireOK(t, putNodes(t, s, evidenceInput{Action: stateport.Action{Actor: "operator"}, RunID: r.ID, Nodes: []stateport.Node{completeNode(s)}}))
			return func(a stateport.Action) error { return completeEvidence(t, s, evidenceInput{Action: a, RunID: r.ID}) }
		}},
		{"ci.project", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed}))
			return func(a stateport.Action) error { return s.ProjectCI(t.Context(), a, r.ID) }
		}},
		{"purge.begin", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed}))
			return func(a stateport.Action) error { return s.BeginPurge(t.Context(), a, r.ID, "purge") }
		}},
		{"purge.finish", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "run")
			requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed}))
			requireOK(t, s.BeginPurge(t.Context(), stateport.Action{Actor: "operator"}, r.ID, "purge"))
			return func(a stateport.Action) error { return s.FinishPurge(t.Context(), a, r.ID, "purge") }
		}},
	})
}
