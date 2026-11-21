package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestBranchDeletionAndNoRefireAfterRestart(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, input := accepted(t, s, "deleted-run")
	request := input.Request
	request.ReceiveID = "branch-delete"
	requireOK(t, recordDeletion(t, s, deletionForTest(request)))
	requireOK(t, recordDeletion(t, s, deletionForTest(request)))
	second, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, second.Close()) }()
	got, err := second.Run(t.Context(), r.ID)
	requireOK(t, err)
	if got.Status != stepmodel.RunCancelled || got.DisplayReason != stateport.ReasonBranchDeleted {
		t.Fatal("deletion lost its state", got.Status)
	}
	obligations, err := second.Obligations(t.Context(), r.ID)
	requireOK(t, err)
	if len(obligations) != 1 {
		t.Fatal("lost cancellation obligation")
	}
	replay, err := acceptForTest(t.Context(), second, input)
	requireOK(t, err)
	if replay.Status != stepmodel.RunCancelled {
		t.Fatal("receive replay refired work")
	}
	requireOK(t, second.Readiness(t.Context()))
	requireOK(t, second.Readiness(t.Context()))
	idle, err := second.Idle(t.Context())
	requireOK(t, err)
	if idle {
		t.Fatal("outstanding obligations reported idle")
	}
}

func TestCompleteEvidenceRerunAndPurgeFences(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, input := accepted(t, s, "failed-source")
	falseValue := false
	n := stateport.Node{NodeID: "node", Path: "build", Attempt: 1, Status: stepmodel.StepFailed, Credential: &falseValue, Exit: new(1)}
	requireOK(t, putNodes(t, s, evidenceInput{Action: stateport.Action{Actor: "observer"}, RunID: r.ID, Nodes: []stateport.Node{n}}))
	requireOK(t, completeEvidence(t, s, evidenceInput{Action: stateport.Action{Actor: "observer"}, RunID: r.ID}))
	updated, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "observer"}, stateport.RunUpdate{ID: r.ID, Revision: updated.Revision, Status: stepmodel.RunFailed}))
	input.RerunOf = r.ID
	view := input.Prepared.View()
	view.RunID = "explicit-rerun"
	input.Prepared, err = admissioncontract.Seal(view)
	requireOK(t, err)
	input.Request.ReceiveID = "rerun-receive"
	next, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	if next.Origin.RerunOf != r.ID {
		t.Fatal("rerun lineage lost")
	}
	requireOK(t, s.BeginPurge(t.Context(), stateport.Action{Actor: "operator"}, r.ID, "purge-one"))
	requireCode(t, s.FinishPurge(t.Context(), stateport.Action{Actor: "operator"}, r.ID, "wrong-receipt"), publicerr.Conflict)
	requireOK(t, s.FinishPurge(t.Context(), stateport.Action{Actor: "operator"}, r.ID, "purge-one"))
	receipt, err := s.PurgeReceipt(t.Context(), r.ID)
	requireOK(t, err)
	if !receipt.Completed || receipt.ID != "purge-one" {
		t.Fatal("purge receipt lost")
	}
	retained, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if retained.ID != r.ID || !retained.EvidenceComplete {
		t.Fatal("purge removed identity or receipt")
	}
	requireCode(t, s.BeginPurge(t.Context(), stateport.Action{Actor: "operator"}, next.ID, "purge-live"), publicerr.Conflict)
}

func TestManualAndCIProjectionVersions(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "red-run")
	requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "observer"}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed}))
	requireOK(t, s.ProjectCI(t.Context(), stateport.Action{Actor: "observer"}, r.ID))
	issues, err := s.Issues(t.Context(), []string{"origin/team/repo"}, model.IssueFilter{Limit: 200})
	requireOK(t, err)
	if len(issues) != 1 || issues[0].CIWorkID != r.ID {
		t.Fatal("CI projection missing")
	}
	requireOK(t, s.ProjectCI(t.Context(), stateport.Action{Actor: "observer"}, r.ID))
	green, _ := accepted(t, s, "green-run")
	requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "observer"}, stateport.RunUpdate{ID: green.ID, Revision: green.Revision, Status: stepmodel.RunPassed}))
	requireOK(t, s.ProjectCI(t.Context(), stateport.Action{Actor: "observer"}, green.ID))
	requireOK(t, s.ProjectCI(t.Context(), stateport.Action{Actor: "observer"}, r.ID))
	issue, err := s.Issue(t.Context(), issues[0].ID)
	requireOK(t, err)
	if issue.State != model.IssueClosed || issue.CIWorkID != green.ID {
		t.Fatal("old red outcome reopened green issue")
	}
	manual, err := s.CreateIssue(t.Context(), stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "manual", Repo: r.Repo, Upstream: "origin"})
	requireOK(t, err)
	requireOK(t, s.CloseIssue(t.Context(), stateport.Action{Actor: "operator"}, stateport.IssueClose{ID: manual.ID, Revision: manual.Revision}))
	requireCode(t, s.CloseIssue(t.Context(), stateport.Action{Actor: "operator"}, stateport.IssueClose{ID: manual.ID, Revision: manual.Revision}), publicerr.Conflict)
	closed, err := s.Issue(t.Context(), manual.ID)
	requireOK(t, err)
	requireOK(t, s.ReopenIssue(t.Context(), stateport.Action{Actor: "operator"}, stateport.IssueClose{ID: manual.ID, Revision: closed.Revision}))
	reopened, err := s.Issue(t.Context(), manual.ID)
	requireOK(t, err)
	requireCode(t, s.DeleteIssue(t.Context(), stateport.Action{Actor: "operator"}, stateport.IssueClose{ID: manual.ID, Revision: closed.Revision}), publicerr.Conflict)
	requireOK(t, s.DeleteIssue(t.Context(), stateport.Action{Actor: "operator"}, stateport.IssueClose{ID: manual.ID, Revision: reopened.Revision}))
	_, err = s.Issue(t.Context(), manual.ID)
	requireCode(t, err, publicerr.NotFound)
	empty, err := s.Issues(t.Context(), []string{}, model.IssueFilter{Limit: 200})
	requireOK(t, err)
	if len(empty) != 0 {
		t.Fatal("issue scope widened")
	}
}

func TestNativeChildProofRefusesIncompleteBinding(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, input := accepted(t, s, "child-source")
	var err error
	child := nativebinding.ChildBinding{}
	input.Child = &child
	input.Request.ReceiveID = "invalid-child"
	view := input.Prepared.View()
	view.RunID = "invalid-child"
	input.Prepared, err = admissioncontract.Seal(view)
	requireOK(t, err)
	if _, err := acceptForTest(t.Context(), s, input); err == nil {
		t.Fatal("incomplete child proof accepted")
	}
	_, err = s.ChildProof(t.Context(), "invalid-child")
	requireCode(t, err, publicerr.NotFound)
	backup, err := NewBackup(filepath.Join(t.TempDir(), "absent.sqlite"))
	requireOK(t, err)
	if _, err := backup.Snapshot(t.Context(), backupOptions(s), &strings.Builder{}); err == nil {
		t.Fatal("missing database created")
	}
}

func TestScheduledChildCommitsAdmissionAndParentProof(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	source, input := accepted(t, s, "child-base")
	repo, err := s.Repository(t.Context(), source.Repo, nil)
	requireOK(t, err)
	parent, err := nativebinding.CronName(repo.ID, "daily")
	requireOK(t, err)
	view := input.Prepared.View()
	view.RunID = parent + "-1"
	view.ScheduleEntry = "daily"
	view.Trigger = model.TriggerSchedule
	input.Prepared, err = admissioncontract.Seal(view)
	requireOK(t, err)
	input.Request.Trigger = view.Trigger
	input.Request.ScheduleEntry = view.ScheduleEntry
	input.Request.ReceiveID = "scheduled-child"
	proof := nativebinding.ChildBinding{Run: nativebinding.RunBinding{RunID: view.RunID, RepositoryID: repo.ID, Ref: view.Ref, Trigger: string(view.Trigger), Revision: sourcecontract.Revision{Repository: source.Repo, Commit: *source.CommitSHA}, Identity: input.Prepared.View().Digest, SpecSHA256: input.Prepared.View().SpecDigest, Workflow: nativebinding.Object{Namespace: "runs", Name: view.RunID, UID: "child-uid"}}, Parent: nativebinding.Object{Namespace: "runs", Name: parent, UID: "parent-uid"}, ScheduleEntry: "daily", CreatedAt: s.options.Clock()}
	input.Child = &proof
	child, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	retained, err := s.ChildProof(t.Context(), child.ID)
	requireOK(t, err)
	if retained != proof || child.Origin.WorkflowUID != "child-uid" || child.Admission.Digest != proof.Run.Identity {
		t.Fatal("child proof not committed with run")
	}
}
