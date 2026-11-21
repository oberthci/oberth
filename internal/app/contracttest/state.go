package contracttest

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type RunsFixture struct {
	Port interface {
		app.Runs
		app.Service
	}
	DuplicateRoot *admissioncontract.PreparedAdmission
	Acceptance    stateport.BatchAcceptance
	Stop          func(string)
}

func Runs(t *testing.T, setup func(*testing.T) RunsFixture) {
	t.Helper()
	for name, check := range map[string]func(*testing.T, RunsFixture){"batch": checkBatch, "receipt_only": checkReceiptOnly, "deletion": checkDeletion, "decided_authority": checkDecidedAuthority, "cancelled": checkCancelled, "invalid_batch": checkInvalidBatch, "duplicate_root": checkDuplicateRoot, "rejected": checkRejected} {
		t.Run(name, func(t *testing.T) { check(t, setup(t)) })
	}
	for _, reason := range []string{stateport.ReasonCreateRefused, stateport.ReasonRetentionRefused, stateport.ReasonRestartAbsent, stateport.ReasonWorkflowOwner} {
		t.Run(reason, func(t *testing.T) { checkUnboundFailure(t, setup(t), reason) })
	}
}
func checkBatch(t *testing.T, f RunsFixture) {
	t.Helper()
	ctx := context.Background()
	row := acceptOne(t, f)
	again, err := f.Port.Accept(ctx, f.Acceptance)
	if err != nil || again.Fresh || len(again.Runs) != 1 || !sameRow(again.Runs[0], row) {
		t.Fatal("replay changed work", again, err)
	}
	read, err := f.Port.AcceptedRuns(ctx, f.Acceptance.ReceiveID)
	if err != nil || len(read) != 1 || !sameRow(read[0], row) {
		t.Fatal("receipt lost", read, err)
	}
	checkDetachedPage(t, f, row, read)
}
func checkDetachedPage(t *testing.T, f RunsFixture, row stateport.Run, read []stateport.Run) {
	t.Helper()
	ctx := context.Background()
	var err error
	read[0].Admission.Workflow.Spec.Entrypoint = "mutated"
	read[0].Origin.Actor = "mutated"
	read, err = f.Port.AcceptedRuns(ctx, f.Acceptance.ReceiveID)
	if err != nil || !sameRow(read[0], row) {
		t.Fatal("stored row aliases reader", read, err)
	}
	page, err := f.Port.UnboundRuns(ctx, 0, 1)
	if err != nil || len(page) != 1 || !sameRow(page[0], row) {
		t.Fatal("full admission not recoverable", page, err)
	}
	page, err = f.Port.UnboundRuns(ctx, row.Sequence, 1)
	if err != nil || len(page) != 0 {
		t.Fatal("cursor did not advance", page, err)
	}
	for _, limit := range []int{0, 201} {
		if _, err := f.Port.UnboundRuns(ctx, 0, limit); err == nil {
			t.Fatal("invalid page accepted", limit)
		}
	}
}
func checkReceiptOnly(t *testing.T, f RunsFixture) {
	t.Helper()
	ctx := context.Background()
	a := f.Acceptance
	a.ReceiveID = "parked"
	a.Runs = nil
	recorded, err := f.Port.Accept(ctx, a)
	if err != nil || !recorded.Fresh || len(recorded.Runs) != 0 {
		t.Fatal("zero-run receipt refused", recorded.Runs, recorded.Fresh, err)
	}
	rows, err := f.Port.AcceptedRuns(ctx, a.ReceiveID)
	if err != nil || len(rows) != 0 {
		t.Fatal("zero-run receipt missing", rows, err)
	}
	recorded, err = f.Port.Accept(ctx, a)
	if err != nil || recorded.Fresh || len(recorded.Runs) != 0 {
		t.Fatal("zero-run replay changed work", recorded.Runs, recorded.Fresh, err)
	}
	row := acceptOne(t, f)
	if row.Repo != a.Repository {
		t.Fatal("next push used another repository")
	}
	a.Actor.Identity = "other"
	if _, err := f.Port.Accept(ctx, a); err == nil {
		t.Fatal("receipt actor changed")
	}
}
func checkUnboundFailure(t *testing.T, f RunsFixture, reason string) {
	t.Helper()
	ctx := context.Background()
	row := acceptOne(t, f)
	input := stateport.UnboundFailure{RunID: row.ID, Reason: reason, Detail: "full diagnostic"}
	action := stateport.Action{Actor: row.Origin.Actor}
	if err := f.Port.FailUnbound(ctx, action, input); err != nil {
		t.Fatal(err)
	}
	if err := f.Port.FailUnbound(ctx, action, input); err != nil {
		t.Fatal("failure retry refused", err)
	}
	read, err := f.Port.AcceptedRuns(ctx, f.Acceptance.ReceiveID)
	if err != nil || len(read) != 1 || read[0].DisplayReason != reason || read[0].Detail != input.Detail || read[0].Status != "failed" {
		t.Fatal(read, err)
	}
	page, err := f.Port.UnboundRuns(ctx, 0, 200)
	if err != nil || len(page) != 0 {
		t.Fatal("failed run remains recoverable", page, err)
	}
}
func checkCancelled(t *testing.T, f RunsFixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Port.Accept(ctx, f.Acceptance); err == nil {
		t.Fatal("cancelled accept wrote receipt")
	}
	if _, err := f.Port.AcceptedRuns(context.Background(), f.Acceptance.ReceiveID); publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("cancelled receipt exists", err)
	}
	row := acceptOne(t, f)
	if err := f.Port.FailUnbound(ctx, stateport.Action{Actor: row.Origin.Actor}, stateport.UnboundFailure{RunID: row.ID, Reason: stateport.ReasonCreateRefused}); err == nil {
		t.Fatal("cancelled failure wrote")
	}
	page, err := f.Port.UnboundRuns(context.Background(), 0, 200)
	if err != nil || len(page) != 1 {
		t.Fatal("cancelled failure lost run", page, err)
	}
}
func checkInvalidBatch(t *testing.T, f RunsFixture) {
	t.Helper()
	ctx := context.Background()
	a := f.Acceptance
	a.Runs = append(a.Runs, a.Runs[0])
	if _, err := f.Port.Accept(ctx, a); err == nil {
		t.Fatal("duplicate run in batch accepted")
	}
	if _, err := f.Port.AcceptedRuns(ctx, a.ReceiveID); publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("partial batch saved", err)
	}
	a = f.Acceptance
	a.Actor.Scope = []string{"other"}
	if _, err := f.Port.Accept(ctx, a); err == nil {
		t.Fatal("batch attribution differs from its runs")
	}
	a = f.Acceptance
	a.Repository = "origin/team/unregistered"
	a.Runs = nil
	if _, err := f.Port.Accept(ctx, a); err == nil {
		t.Fatal("unknown repository accepted")
	}
	acceptOne(t, f)
}
func acceptOne(t *testing.T, f RunsFixture) stateport.Run {
	t.Helper()
	recorded, err := f.Port.Accept(context.Background(), f.Acceptance)
	if err != nil || !recorded.Fresh || len(recorded.Runs) != 1 {
		t.Fatal("accept failed", recorded.Runs, recorded.Fresh, err)
	}
	return recorded.Runs[0]
}

func sameRow(a, b stateport.Run) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func checkDuplicateRoot(t *testing.T, f RunsFixture) {
	t.Helper()
	a := f.Acceptance
	a.Runs = append([]stateport.Acceptance(nil), a.Runs...)
	if f.DuplicateRoot == nil {
		t.Fatal("duplicate-root fixture missing")
	}
	a.Runs[0].Prepared = f.DuplicateRoot
	if _, err := f.Port.Accept(t.Context(), a); err == nil {
		t.Fatal("duplicate logical root path accepted")
	}
	if _, err := f.Port.AcceptedRuns(t.Context(), a.ReceiveID); publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("invalid plan committed receipt", err)
	}
	acceptOne(t, f)
}

func checkRejected(t *testing.T, f RunsFixture) {
	t.Helper()
	a := f.Acceptance
	a.Runs = append([]stateport.Acceptance(nil), a.Runs...)
	id := a.Runs[0].Prepared.View().RunID
	finding := admissioncontract.Finding{Code: "grant", Path: "spec.templates[build].secrets[kv/build]", Reason: "credential is not granted", Remedy: "Approve this exact declaration.", Command: "oberth access allow 'origin/team/widget' '.oberth/build.yaml' 'build' 'kv/build'"}
	a.Runs[0].Prepared = nil
	a.Runs[0].Rejected = &stateport.RejectedAdmission{ID: id, Findings: []admissioncontract.Finding{finding}, Failure: stepmodel.FailureRecord{Class: stepmodel.FailureAuthor, Retry: stepmodel.RetryHint{Action: stepmodel.RetryRerun}}}
	recorded, err := f.Port.Accept(t.Context(), a)
	if err != nil || !recorded.Fresh || len(recorded.Runs) != 1 {
		t.Fatal("rejected batch refused", recorded.Runs, recorded.Fresh, err)
	}
	rows, err := f.Port.AcceptedRuns(t.Context(), a.ReceiveID)
	if err != nil || len(rows) != 1 || rows[0].HasAdmission || len(rows[0].Findings) != 1 {
		t.Fatal("rejection read invented admission", rows, err)
	}
	if rows[0].Findings[0] != finding {
		t.Fatal("rejection dropped typed finding/command", rows[0].Findings)
	}
	rows[0].Findings[0].Command = "mutated"
	checkRejectedReplay(t, f, a, finding)
}
func checkRejectedReplay(t *testing.T, f RunsFixture, a stateport.BatchAcceptance, finding admissioncontract.Finding) {
	t.Helper()
	rows, err := f.Port.AcceptedRuns(t.Context(), a.ReceiveID)
	if err != nil || rows[0].Findings[0] != finding {
		t.Fatal("rejection aliases reader", rows, err)
	}
	if recorded, err := f.Port.Accept(t.Context(), a); err != nil || recorded.Fresh {
		t.Fatal("rejection replay created new work", err)
	}
}
