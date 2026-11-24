package app_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestStarterManualRerunRetainsRequestedFile(t *testing.T) {
	f := newFixture(t)
	db := realStore(t)
	f.d.Runs = db
	f.d.Service = storeService{Store: db, Policy: f.runs}
	q := request()
	q.File = ".oberth/walk.yaml"
	prepared, err := f.new(t).Admit(t.Context(), q, f.source)
	if err != nil {
		t.Fatal(err)
	}
	original := batch(prepared, "original")
	original.Runs[0].Request.RequestedFile = q.File
	if _, err := db.AcceptBatch(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	if err := db.FailUnbound(t.Context(), stateport.Action{Actor: q.Actor.Identity}, stateport.UnboundFailure{RunID: q.RunID, Reason: stateport.ReasonCreateRefused}); err != nil {
		t.Fatal(err)
	}
	f.d.Resolver = fake.StartResolver{Resolve: func(ctx context.Context, start service.StartRequest) (app.Request, error) {
		r, err := resolveStart(ctx, start)
		if err != nil {
			return r, err
		}
		old, err := db.Run(ctx, start.RerunOf)
		r.RunID, r.Original = "rerun", &old
		return r, err
	}}
	start := startRequest("manual-rerun", request())
	start.RerunOf = q.RunID
	s := f.new(t)
	for range 2 {
		if _, err := s.StartRun(t.Context(), start); err != nil {
			t.Fatal(err)
		}
	}
	row, err := db.Run(t.Context(), "rerun")
	if err != nil || row.Origin.RequestedFile != q.File || row.Admission.SelectedFile != q.File || row.Origin.RerunOf != q.RunID {
		t.Fatal("rerun lost the requested file", row.Origin, err)
	}
}
