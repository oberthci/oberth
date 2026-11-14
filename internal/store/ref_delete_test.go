package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func branchDeleteEvent(repo int64, id, ref string) model.ReceiveEventSpec {
	return model.ReceiveEventSpec{ID: id, Actor: "developer", RepoID: repo, RefKind: model.RefBranch, Ref: ref, OldSHA: strings.Repeat("a", 40), Outcome: "deleted"}
}

func TestRefDeletionCancelsAllUnfinishedBranchPhases(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	var cancelled, untouched []model.Run
	for _, cell := range []struct {
		name, status, phase, trigger string
		kind                         model.RefKind
		job, assist, cancel          bool
	}{
		{"queued", "queued", "queued", "branch", model.RefBranch, false, false, true},
		{"preparing", "running", "waiting", "branch", model.RefBranch, false, false, true},
		{"running", "running", "running", "branch", model.RefBranch, true, false, true},
		{"preflight", "running", "preflight", "branch", model.RefBranch, true, false, true},
		{"assisted", "running", "running", "branch", model.RefBranch, true, true, true},
		{"pending-create", "running", "infrastructure-pending", "branch", model.RefBranch, true, false, true},
		{"schedule", "queued", "queued", "schedule", model.RefBranch, false, false, true},
		{"promotion", "running", "running", "promotion", model.RefBranch, true, false, false},
		{"tag", "running", "running", "tag", model.RefTag, true, false, false},
		{"passed", "passed", "passed", "branch", model.RefBranch, true, false, false},
		{"other-ref", "queued", "queued", "branch", model.RefBranch, false, false, false},
	} {
		spec := testRunSpec(repo.ID, cell.name, strings.Repeat("a", 40))
		spec.RefKind = cell.kind
		spec.Trigger = cell.trigger
		spec.Assist = cell.assist
		if cell.trigger == "schedule" {
			spec.ScheduleEntry = "nightly"
		}
		run, err := s.EnqueueRun(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		ref := "feature/deleted"
		if cell.name == "other-ref" {
			ref = "feature/other"
		}
		job := ""
		if cell.job {
			job = "workflow-" + cell.name
		}
		if _, err = s.db.Exec(`UPDATE runs SET ref=?,status=?,phase=?,job_name=?,error='retained create receipt' WHERE id=?`, ref, cell.status, cell.phase, job, run.ID); err != nil {
			t.Fatal(err)
		}
		current, err := s.Run(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cell.cancel {
			cancelled = append(cancelled, current)
		} else {
			untouched = append(untouched, current)
		}
	}
	if _, err := s.RecordReceiveEvent(t.Context(), branchDeleteEvent(repo.ID, "delete", "feature/deleted")); err != nil {
		t.Fatal(err)
	}
	for _, before := range cancelled {
		run, err := s.Run(t.Context(), before.ID)
		if err != nil || !run.Cancelled() || run.Reason != "ref deleted" || (before.Status == model.RunQueued && run.FinishedAt == nil) || (before.Status == model.RunRunning && (run.Status != model.RunRunning || run.FinishedAt != nil)) {
			t.Fatalf("run not cancelled: %+v %v", run, err)
		}
		if before.Phase == "infrastructure-pending" && run.Error != before.Error {
			t.Fatal("lost late-create receipt")
		}
	}
	for _, before := range untouched {
		run, err := s.Run(t.Context(), before.ID)
		if err != nil || run.Status != before.Status || run.Phase != before.Phase || run.Reason != before.Reason {
			t.Fatalf("unrelated run changed: %+v %v", run, err)
		}
	}
	pending, err := s.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 5 {
		t.Fatalf("cleanup obligations: %+v %v", pending, err)
	}
	// A name published after cancellation still acquires the existing obligation.
	late := cancelled[1]
	if _, err = s.SetRunWorkflowName(t.Context(), late.ID, "late-workflow"); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingRunCancellations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range pending {
		if c.RunID == late.ID && c.WorkflowName == "late-workflow" {
			found = true
		}
	}
	if !found {
		t.Fatal("late workflow lost its cancellation")
	}
}

func TestRefDeletionReplayCannotCancelRecreatedBranchAtSameSHA(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	event, spec := nativeReceive(repo.ID, "old", "branch")
	event.Ref = "feature/recreated"
	spec.Ref = event.Ref
	old, err := s.EnqueueReceiveEvent(t.Context(), event, spec)
	if err != nil {
		t.Fatal(err)
	}
	deletion := branchDeleteEvent(repo.ID, "delete", event.Ref)
	if _, err = s.RecordReceiveEvent(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}
	// This recreation has the identical commit and clock value as the old run.
	event.ID = "new"
	event.OldSHA = ""
	newer, err := s.EnqueueReceiveEvent(t.Context(), event, spec)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := s.RecordReceiveEvent(t.Context(), deletion); err != nil || !duplicate {
		t.Fatalf("delete replay: %v %v", duplicate, err)
	}
	current, err := s.Run(t.Context(), newer.ID)
	if err != nil || current.Status != model.RunRunning || current.Reason == "ref deleted" {
		t.Fatalf("new push cancelled: %+v %v", current, err)
	}
	before, err := s.Run(t.Context(), old.ID)
	if err != nil || !before.Cancelled() {
		t.Fatalf("old run survived: %+v %v", before, err)
	}
}

func TestRefDeletionAuditFailureRollsBackCancellation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/deleted", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER refuse_delete_audit BEFORE INSERT ON audit_actions BEGIN SELECT RAISE(ABORT,'audit refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordReceiveEvent(t.Context(), branchDeleteEvent(repo.ID, "delete", run.Ref)); err == nil {
		t.Fatal("unaudited deletion accepted")
	}
	current, err := s.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning {
		t.Fatalf("failed transaction cancelled run: %+v %v", current, err)
	}
	if _, err = s.ReceiveEvent(t.Context(), "delete"); err == nil {
		t.Fatal("failed transaction retained receipt")
	}
}

func TestRefDeletionLeavesPendingPublicationOwnerUntouched(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	spec := testRunSpec(repo.ID, "feature/publishing", strings.Repeat("a", 40))
	run, err := s.EnqueueRun(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref, PreviousSHA: strings.Repeat("b", 40), ResultSHA: run.SHA, Actor: spec.Actor}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordReceiveEvent(t.Context(), branchDeleteEvent(repo.ID, "delete", run.Ref)); err != nil {
		t.Fatal(err)
	}
	current, err := s.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning || current.Phase != "publishing" {
		t.Fatalf("pending publication owner cancelled: %+v %v", current, err)
	}
}

func nativeReceive(repo int64, id, trigger string) (model.ReceiveEventSpec, model.RunSpec) {
	sha := strings.Repeat("a", 40)
	e := model.ReceiveEventSpec{ID: id, Actor: "dev", RepoID: repo, RefKind: model.RefBranch, Ref: "feature/" + id, ObjectSHA: sha, CommitSHA: sha, Outcome: "queued"}
	return e, model.RunSpec{RepoID: repo, Actor: e.Actor, RefKind: e.RefKind, Ref: e.Ref, SHA: sha, TestedSHA: sha, Trigger: trigger}
}
