package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func accepted(t *testing.T, s *Store, id string) (stateport.Run, stateport.Acceptance) {
	t.Helper()
	return acceptedMode(t, s, id, false)
}
func acceptedMode(t *testing.T, s *Store, id string, assist bool) (stateport.Run, stateport.Acceptance) {
	t.Helper()
	requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	_, err := s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, "origin/team/repo")
	requireOK(t, err)
	sha := strings.Repeat("a", 40)
	request := model.CIRequest{ReceiveID: "receive-" + id, Repo: "origin/team/repo", Ref: "refs/heads/main", ObjectSHA: sha, CommitSHA: &sha, Actor: actor.Actor{Identity: "operator", AuthMethod: "cert"}, Trigger: model.TriggerBranch, Assist: assist}
	view := admissioncontract.PreparedView{RunID: id, Repository: request.Repo, Ref: request.Ref, ObjectSHA: sha, CommitSHA: sha, Trigger: request.Trigger, Workflow: &wfv1.Workflow{}, Plan: admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build"}}}}
	prepared, err := admissioncontract.Seal(view)
	requireOK(t, err)
	input := stateport.Acceptance{Request: request, Prepared: prepared}
	run, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	return run, input
}

func TestStoredAdmissionRestoresKeepRoots(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	run, input := accepted(t, s, "run-one")
	repo, err := s.Repository(t.Context(), run.Repo, nil)
	requireOK(t, err)
	binding := nativebinding.RunBinding{RunID: run.ID, RepositoryID: repo.ID, Revision: sourcecontract.Revision{Repository: run.Repo, Commit: *run.CommitSHA}, Ref: run.Ref, Trigger: string(run.Origin.Trigger), Identity: run.Admission.Digest, SpecSHA256: run.Admission.SpecDigest, Workflow: nativebinding.Object{Namespace: "runs", Name: "workflow-one", UID: "uid-one"}}
	requireOK(t, s.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, binding))
	beforeBinding := independentHead(t, s.readers)
	requireOK(t, s.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, binding))
	if independentHead(t, s.readers) != beforeBinding {
		t.Fatal("binding replay changed audit")
	}
	var archive strings.Builder
	_, err = s.Snapshot(t.Context(), backupOptions(s), &archive)
	requireOK(t, err)
	path := filepath.Join(t.TempDir(), "restored.sqlite")
	_, err = backupForTest(t, s).Restore(t.Context(), restoreRequest(s, path), strings.NewReader(archive.String()))
	requireOK(t, err)
	restored, err := Open(t.Context(), path, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, restored.Close()) }()
	got, err := restored.Run(t.Context(), run.ID)
	requireOK(t, err)
	if got.Admission.Digest != run.Admission.Digest || encode(got.Admission.SourceReceipt) != encode(input.Prepared.View().SourceReceipt) || got.Origin.WorkflowUID != "uid-one" {
		t.Fatal("admission or native root changed")
	}
	binding.Workflow.UID = "replacement"
	requireCode(t, restored.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, binding), publicerr.Conflict)
}

func restoreRequest(s *Store, path string) stateport.RestoreRequest {
	return stateport.RestoreRequest{Options: backupOptions(s), Destination: path}
}

func TestAcceptedReceiveReplayAndChangedBinding(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	run, input := accepted(t, s, "receipt-one")
	head := independentHead(t, s.readers)
	again, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	if again.ID != run.ID || independentHead(t, s.readers) != head {
		t.Fatal("replay changed state")
	}
	input.Request.Assist = !input.Request.Assist
	_, err = acceptForTest(t.Context(), s, input)
	requireCode(t, err, publicerr.Conflict)
}

func TestEvidenceUnknownAndNondeclassification(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "evidence-one")
	requireOK(t, s.SaveProgress(t.Context(), r.ID, stateport.Progress{Steps: []stepmodel.RunStep{{Path: "build", Status: stepmodel.StepPending}}}))
	credential := true
	n := stateport.Node{NodeID: "node-one", Path: "build", Attempt: 1, Status: stepmodel.StepRunning, Credential: &credential}
	requireOK(t, putNodes(t, s, evidenceInput{Action: stateport.Action{Actor: "observer"}, RunID: r.ID, Nodes: []stateport.Node{n}}))
	n.Credential = new(false)
	requireCode(t, putNodes(t, s, evidenceInput{Action: stateport.Action{Actor: "observer"}, RunID: r.ID, Nodes: []stateport.Node{n}}), publicerr.Conflict)
	n.Credential = &credential
	n.Status = stepmodel.StepPassed
	requireOK(t, putNodes(t, s, evidenceInput{Action: stateport.Action{Actor: "observer"}, RunID: r.ID, Nodes: []stateport.Node{n}}))
	steps, err := s.Steps(t.Context(), r.ID)
	requireOK(t, err)
	if steps[0].Status != stepmodel.StepPending {
		t.Fatal("raw evidence overwrote the saved plan projection")
	}
	requireCode(t, completeEvidence(t, s, evidenceInput{Action: stateport.Action{Actor: "observer"}, RunID: r.ID}), publicerr.Conflict)
}

func TestEvidenceBatchesDoNotImposeNodeCeiling(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, input := accepted(t, s, "many-nodes")
	view := input.Prepared.View()
	view.RunID = "dynamic-nodes"
	view.Plan = admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "loop", Dynamic: true}}}
	prepared, err := admissioncontract.Seal(view)
	requireOK(t, err)
	input.Prepared = prepared
	input.Request.ReceiveID = "dynamic-receive"
	r, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	nodes := make([]stateport.Node, 129)
	for i := range nodes {
		nodes[i] = stateport.Node{NodeID: fmt.Sprintf("node-%d", i), Path: fmt.Sprintf("loop/%d", i), Attempt: 1, Status: stepmodel.StepRunning}
	}
	requireCode(t, putNodes(t, s, evidenceInput{Action: stateport.Action{Actor: "observer"}, RunID: r.ID, Nodes: nodes}), publicerr.InvalidInput)
	requireOK(t, putNodes(t, s, evidenceInput{Action: stateport.Action{Actor: "observer"}, RunID: r.ID, Nodes: nodes[:128]}))
	execute(t, s.writer, `CREATE TRIGGER fail_last BEFORE INSERT ON nodes WHEN NEW.node_id='node-128' BEGIN SELECT RAISE(ABORT,'last batch failed'); END`)
	if err := putNodes(t, s, evidenceInput{Action: stateport.Action{Actor: "observer"}, RunID: r.ID, Nodes: nodes[128:]}); err == nil {
		t.Fatal("failure did not fire")
	}
	got, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if got.EvidenceComplete {
		t.Fatal("partial batches claimed complete evidence")
	}
	var count int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM nodes WHERE run_id=?`, r.ID).Scan(&count))
	if count != 128 {
		t.Fatal("batch boundary", count)
	}
	execute(t, s.writer, `DROP TRIGGER fail_last`)
	requireOK(t, putNodes(t, s, evidenceInput{Action: stateport.Action{Actor: "observer"}, RunID: r.ID, Nodes: nodes[128:]}))
	got, err = s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if got.Running != 0 || got.Pending != 1 || got.Exact {
		t.Fatal("raw evidence became an alternate step projector", got.LogicalStepCounts)
	}
}
