package app_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestMixedPushCommitsDeletionAndAdmissionOnce(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"recording_spy", "store"} {
		t.Run(kind, func(t *testing.T) { mixedPush(t, kind) })
	}
}
func mixedPush(t *testing.T, kind string) {
	t.Helper()
	f := newFixture(t)
	publication := f.runs.Publication
	if kind == "store" {
		db := realStore(t)
		f.d.Runs = db
		f.d.Service = storeService{Store: db, Policy: f.runs}
		publication = db.Publication
	}
	recorder := &mixedRecorder{Service: f.d.Service}
	f.d.Service = recorder
	s := f.new(t)
	first := f.gate(t, s)
	if _, err := s.Deliver(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	f.runs.DecideFunc = deletionDecision
	bad := mixedReceive("bad", "r1")
	if _, err := s.Gate(t.Context(), bad); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deliver(t.Context(), bad); err == nil {
		t.Fatal("duplicate run accepted in mixed push")
	}
	assertDeletionRollback(t, f, first.ID, publication)
	r := mixedReceive("mixed", "r2")
	if _, err := s.Gate(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	checks := f.prepares.Load()
	f.runs.DecideFunc = func(context.Context, service.RefUpdate) (service.Decision, error) {
		t.Error("policy checked after gate")
		return service.Decision{}, nil
	}
	result, err := s.Deliver(t.Context(), r)
	assertMixedResult(t, result, err)
	assertRecordedDeletion(t, f, first.ID, publication)
	calls := recorder.calls
	if _, err := s.Deliver(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != calls || f.prepares.Load() != checks || f.creates.Load() != 2 {
		t.Fatal("replay repeated mixed push", recorder.calls, f.prepares.Load(), f.creates.Load())
	}
	if recorder.mixed != 2 {
		t.Fatal("deletion and admission did not share one service call", recorder.mixed)
	}
}
func mixedReceive(id, runID string) app.Receive {
	r := receive()
	r.ID = id
	d := r.Updates[0]
	d.Request.RunID = "deletion"
	d.PreviousSHA = d.Request.Ref.ObjectSHA
	d.Request.Ref = sourcecontract.Ref{Name: d.Request.Ref.Name}
	d.Deleted = true
	r.Updates[0].Request.RunID = runID
	r.Updates[0].Request.Ref.Name = "refs/heads/next"
	r.Updates = append([]app.RefUpdate{d}, r.Updates...)
	r.Source = newSource()
	return r
}
func deletionDecision(_ context.Context, u service.RefUpdate) (service.Decision, error) {
	d := decision(u, "")
	if u.ObjectSHA == "" {
		d.Action = "deleted"
		d.Request.CommitSHA = nil
		d.Request.PreviousSHA = u.PreviousSHA
		d.Deletion = &stateport.BranchDeletion{Request: d.Request.Clone(), Publication: model.Publication{ID: "deletion", Repo: u.Repo, Ref: u.Ref, Actor: u.Actor.Identity, PreviousSHA: new(u.PreviousSHA)}}
	}
	return d, nil
}
func assertDeletionRollback(t *testing.T, f *fixture, receiveID string, publication func(context.Context, string) (model.Publication, error)) {
	t.Helper()
	rows, err := f.d.Runs.AcceptedRuns(t.Context(), receiveID)
	if err != nil || len(rows) != 1 || rows[0].Status != "queued" {
		t.Fatal("partial mixed deletion", rows, err)
	}
	if _, err := publication(t.Context(), "deletion"); publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("failed mixed push retained publication", err)
	}
	if _, err := f.d.Runs.AcceptedRuns(t.Context(), "bad"); publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("failed mixed push retained receipt", err)
	}
}
func assertRecordedDeletion(t *testing.T, f *fixture, receiveID string, publication func(context.Context, string) (model.Publication, error)) {
	t.Helper()
	rows, err := f.d.Runs.AcceptedRuns(t.Context(), receiveID)
	if err != nil || len(rows) != 1 || rows[0].Status != "cancelled" || rows[0].DisplayReason != stateport.ReasonBranchDeleted {
		t.Fatal(rows, err)
	}
	stops, err := f.d.Runs.Obligations(t.Context(), "r1")
	if err != nil || len(stops) != 1 || stops[0].Value != "Stop" {
		t.Fatal("deletion lost stop", stops, err)
	}
	p, err := publication(t.Context(), "deletion")
	assertDeletionPublication(t, p, err)
}
func assertDeletionPublication(t *testing.T, p model.Publication, err error) {
	t.Helper()
	if err != nil || p.RunID != "" || p.ObjectSHA != "" || p.CommitSHA != nil || p.PreviousSHA == nil || *p.PreviousSHA != request().Ref.ObjectSHA || p.Actor != request().Actor.Identity || p.Ref != request().Ref.Name {
		t.Fatal("deletion lost exact intent", p, err)
	}
}

type mixedRecorder struct {
	app.Service
	calls, mixed int
}

func (s *mixedRecorder) Accept(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	s.calls++
	if len(b.Deletions) > 0 {
		if len(b.Runs) != 1 {
			return stateport.AcceptanceResult{}, publicerr.New(publicerr.InvalidInput, "mixed batch lost its run", "Pass the complete batch.", nil)
		}
		s.mixed++
	}
	return s.Service.Accept(ctx, b)
}

func assertMixedResult(t *testing.T, result app.PushResult, err error) {
	t.Helper()
	if err != nil || len(result.Outcomes) != 2 || result.Outcomes[0].State != "deleted" || result.Outcomes[1].State != "started" {
		t.Fatal(result, err)
	}
}

func TestDeletionOnlyPushRecordsReceiptWithoutPreparing(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"recording_spy", "store"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			publication := f.runs.Publication
			if kind == "store" {
				db := realStore(t)
				f.d.Runs = db
				f.d.Service = storeService{Store: db, Policy: f.runs}
				publication = db.Publication
			}
			f.runs.DecideFunc = deletionDecision
			s := f.new(t)
			r := mixedReceive("deletion-only", "")
			r.Updates = r.Updates[:1]
			if _, err := s.Gate(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Deliver(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			rows, err := f.d.Runs.AcceptedRuns(t.Context(), r.ID)
			if err != nil || len(rows) != 0 || f.prepares.Load() != 0 || f.creates.Load() != 0 {
				t.Fatal("deletion was not a receipt-only batch", rows, err)
			}
			if _, err := publication(t.Context(), "deletion"); err != nil {
				t.Fatal("deletion intent missing", err)
			}
			if _, err := s.Deliver(t.Context(), r); err != nil {
				t.Fatal("deletion receipt not replayable", err)
			}
		})
	}
}
