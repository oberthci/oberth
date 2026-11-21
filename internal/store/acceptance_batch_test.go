package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func batchFixture(t *testing.T, s *Store) stateport.BatchAcceptance {
	t.Helper()
	_, base := accepted(t, s, "source")
	first := reacceptance(t, base, "batch-a")
	second := reacceptance(t, base, "batch-b")
	view := second.Prepared.View()
	view.Ref = "refs/heads/dev"
	second.Prepared = sealView(t, view)
	second.Request.Ref = view.Ref
	first.Request.ReceiveID = "whole-push"
	second.Request.ReceiveID = first.Request.ReceiveID
	first.Events = []stateport.EventWrite{atomicEvent(s, "batch-first")}
	second.Events = []stateport.EventWrite{atomicEvent(s, "batch-second")}
	first.Actions = []model.AuditAction{{Actor: "operator", Action: "submit.binding", ResourceType: "run", ResourceID: "batch-a", Details: `{"fingerprint":"selected"}`}}
	return stateport.BatchAcceptance{ReceiveID: first.Request.ReceiveID, Repository: first.Request.Repo, Actor: first.Request.Actor, Runs: []stateport.Acceptance{first, second}}
}

func TestReceiveBatchCommitsAndReplaysEveryRun(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	input := batchFixture(t, s)
	before := independentHead(t, s.readers)
	recorded, err := s.AcceptBatch(t.Context(), input)
	runs, fresh := recorded.Runs, recorded.Fresh
	requireOK(t, err)
	if !fresh || len(runs) != 2 || runs[0].ID != "batch-a" || runs[1].ID != "batch-b" {
		t.Fatal("incomplete push acceptance", runs, fresh)
	}
	head := independentHead(t, s.readers)
	if head.ID != before.ID+3 {
		t.Fatal("per-run and submit audits not atomic", head)
	}
	replayed, err := s.AcceptBatch(t.Context(), input)
	again, fresh := replayed.Runs, replayed.Fresh
	requireOK(t, err)
	if fresh || len(again) != 2 || independentHead(t, s.readers) != head {
		t.Fatal("receive replay created work")
	}
	read, err := s.AcceptedRuns(t.Context(), input.ReceiveID)
	requireOK(t, err)
	if len(read) != 2 || read[1].ID != runs[1].ID {
		t.Fatal("receive lost its ordered results", read)
	}
	page, err := s.Events(t.Context(), stateport.EventFilter{Limit: 200})
	requireOK(t, err)
	if len(page.Events) != 2 {
		t.Fatal("push lost its events")
	}
	input.Runs[1].Request.Assist = !input.Runs[1].Request.Assist
	_, err = s.AcceptBatch(t.Context(), input)
	requireCode(t, err, publicerr.Conflict)
}

func TestReceiveBatchRollsBackOnLaterRunOrAuditFailure(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"run", "audit"} {
		t.Run(point, func(t *testing.T) {
			s := testStore(t)
			input := batchFixture(t, s)
			before := databaseContents(t, s.readers)
			trigger := `CREATE TRIGGER reject_batch BEFORE INSERT ON runs WHEN NEW.id='batch-b' BEGIN SELECT RAISE(ABORT,'batch failed'); END`
			if point == "audit" {
				trigger = `CREATE TRIGGER reject_batch BEFORE INSERT ON audit WHEN NEW.action='submit.binding' BEGIN SELECT RAISE(ABORT,'batch failed'); END`
			}
			execute(t, s.writer, trigger)
			if recorded, err := s.AcceptBatch(t.Context(), input); err == nil || recorded.Fresh || len(recorded.Runs) != 0 {
				t.Fatal("failed batch returned accepted work", recorded, err)
			}
			if databaseContents(t, s.readers) != before {
				t.Fatal("partial push retained run, receipt, attribution, audit or event")
			}
		})
	}
}

func TestReceiveBatchEventFailureIsAtomic(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{{"receive.batch", func(t *testing.T, s *Store) func(stateport.Action) error {
		input := batchFixture(t, s)
		input.Runs[1].Events = nil
		return func(a stateport.Action) error {
			input.Runs[0].Events = a.Events
			_, err := s.AcceptBatch(t.Context(), input)
			return err
		}
	}}})
}

func rejectedFixture(t *testing.T, s *Store) stateport.Acceptance {
	t.Helper()
	_, input := accepted(t, s, "source")
	input.Prepared = nil
	input.Request.ReceiveID = "refused-receive"
	input.Rejected = &stateport.RejectedAdmission{ID: "refused-run", Failure: stepmodel.FailureRecord{Class: stepmodel.FailureAuthor, Retry: stepmodel.RetryHint{Action: stepmodel.RetryRerun}}, Findings: []admissioncontract.Finding{{Path: "pipeline", Code: "invalid", Reason: "pipeline is invalid", Remedy: "Fix the reported pipeline and push again."}}}
	return input
}

func TestRejectedReceiveHasFindingsAndNoExecutableAdmission(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	input := rejectedFixture(t, s)
	r, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	assertRejectedRun(t, r)
	var absent bool
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT admission IS NULL FROM runs WHERE id=?`, r.ID).Scan(&absent))
	if !absent {
		t.Fatal("rejected receive fabricated admission bytes")
	}
	_, err = s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: r.ID, Nodes: 1})
	requireCode(t, err, publicerr.InvalidTransition)
	source, err := s.Run(t.Context(), "source")
	requireOK(t, err)
	binding := runBinding(source)
	binding.RunID = r.ID
	requireCode(t, s.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, binding), publicerr.InvalidTransition)
	var archive bytes.Buffer
	_, err = s.Snapshot(t.Context(), backupOptions(s), &archive)
	requireOK(t, err)
	destination := filepath.Join(t.TempDir(), "restored.sqlite")
	_, err = backupForTest(t, s).Restore(t.Context(), restoreRequest(s, destination), bytes.NewReader(archive.Bytes()))
	requireOK(t, err)
	restored, err := Open(t.Context(), destination, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, restored.Close()) }()
	got, err := restored.Run(t.Context(), r.ID)
	requireOK(t, err)
	assertRejectedRun(t, got)
}

func assertRejectedRun(t *testing.T, r stateport.Run) {
	t.Helper()
	if r.HasAdmission || r.Admission.Digest != "" || r.Admission.Workflow != nil || len(r.Admission.Plan.Entries) != 0 || r.Status != stepmodel.RunFailed || len(r.Findings) != 1 || r.DisplayReason != stateport.ReasonAdmission {
		t.Fatal("rejected receive invented executable work or lost findings", r)
	}
}

func TestReceiveBatchRejectsMixedAuthorityAndMissingContext(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	input := batchFixture(t, s)
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: "empty"})
	requireCode(t, err, publicerr.InvalidInput)
	input.Runs[1].Request.Actor.Identity = "other"
	_, err = s.AcceptBatch(t.Context(), input)
	requireCode(t, err, publicerr.InvalidInput)
	_, err = s.AcceptedRuns(t.Context(), input.ReceiveID)
	requireCode(t, err, publicerr.NotFound)
}

func TestFailedAdmissionAfterCrashReplayIsDurableAndIdempotent(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	input := rejectedFixture(t, s)
	input.Events = []stateport.EventWrite{atomicEvent(s, "crash-replay-refusal")}
	path, options := s.path, s.options
	requireOK(t, s.Close())
	restarted, err := Open(t.Context(), path, options)
	requireOK(t, err)
	batch := stateport.BatchAcceptance{ReceiveID: input.Request.ReceiveID, Repository: input.Request.Repo, Actor: input.Request.Actor, Runs: []stateport.Acceptance{input}}
	restoredResult, err := restarted.AcceptBatch(t.Context(), batch)
	runs, fresh := restoredResult.Runs, restoredResult.Fresh
	requireOK(t, err)
	if !fresh || len(runs) != 1 {
		t.Fatal("crash replay lost its failed acceptance", runs, fresh)
	}
	assertRejectedRun(t, runs[0])
	head := independentHead(t, restarted.readers)
	requireOK(t, restarted.Close())
	reopened, err := Open(t.Context(), path, options)
	requireOK(t, err)
	defer func() { requireOK(t, reopened.Close()) }()
	replayResult, err := reopened.AcceptBatch(t.Context(), batch)
	runs, fresh = replayResult.Runs, replayResult.Fresh
	requireOK(t, err)
	if fresh || len(runs) != 1 || independentHead(t, reopened.readers) != head {
		t.Fatal("failed receive replay created new work or audit", runs, fresh)
	}
	assertRejectedRun(t, runs[0])
	page, err := reopened.Events(t.Context(), stateport.EventFilter{Limit: 200})
	requireOK(t, err)
	if len(page.Events) != 1 {
		t.Fatal("failed receive replay duplicated its event", page)
	}
}
