package app_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/contracttest"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/store/storetest"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

var _ app.Runs = (*store.Store)(nil)

func TestStoreOwnerPortContract(t *testing.T) {
	t.Parallel()
	contracttest.Runs(t, func(t *testing.T) contracttest.RunsFixture {
		f := newFixture(t)
		p := mustAdmit(t, f.new(t), f)
		s := realStore(t)
		return contracttest.RunsFixture{Port: storeService{Store: s, Policy: f.runs}, DuplicateRoot: duplicateRoot(t, p), Acceptance: batch(p, "ports"), Stop: func(value string) {
			run, err := s.Run(t.Context(), "r1")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Cancel(t.Context(), stateport.Action{Actor: request().Actor.Identity}, stateport.RunCancellation{ID: run.ID, Revision: run.Revision, Terminate: value == "Terminate"})
			if err != nil {
				t.Fatal(err)
			}
		}}
	})
}
func realStoreOptions() store.Options {
	return store.Options{Protocol: stateport.DefaultProtocol(), BusyTimeout: time.Second, Clock: func() time.Time { return time.Unix(100, 0) }}
}

//nolint:gochecknoglobals // Tests open independent files copied from one closed baseline.
var baselineDatabase = storetest.New(func(t *testing.T, path string) {
	s, err := store.Create(t.Context(), path, realStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
})

func realStore(t *testing.T) *store.Store {
	t.Helper()
	options := realStoreOptions()
	s, err := store.Open(t.Context(), baselineDatabase.Path(t), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	a := stateport.Action{Actor: request().Actor.Identity}
	if err := s.RegisterUpstream(t.Context(), a, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRepository(t.Context(), a, request().Repository); err != nil {
		t.Fatal(err)
	}
	return s
}
func TestParkedReceiptAllowsNextPushToSameRepository(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"recording_spy", "store"} {
		t.Run(kind, func(t *testing.T) { parkThenPush(t, kind) })
	}
}
func parkThenPush(t *testing.T, kind string) {
	t.Helper()
	f := newFixture(t)
	if kind == "store" {
		f.d.Runs = realStore(t)
		f.d.Service = storeService{Store: f.d.Runs.(*store.Store), Policy: f.runs}
	}
	parked := true
	f.runs.DecideFunc = func(_ context.Context, u service.RefUpdate) (service.Decision, error) {
		d := decision(u, "")
		if parked {
			d.Action = "parked"
		}
		return d, nil
	}
	s := f.new(t)
	r := f.gate(t, s)
	result, err := s.Deliver(context.Background(), r)
	if err != nil || result.Outcomes[0].State != "parked" || f.creates.Load() != 0 {
		t.Fatal(result, err)
	}
	if rows, err := f.d.Runs.AcceptedRuns(t.Context(), r.ID); err != nil || len(rows) != 0 {
		t.Fatal("parked receipt missing", rows, err)
	}
	parked = false
	r.ID = "next"
	r.Updates[0].Request.RunID = "r2"
	if _, err := s.Gate(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	result, err = s.Deliver(t.Context(), r)
	if err != nil || result.Outcomes[0].State != "started" || f.creates.Load() != 1 {
		t.Fatal("parked receipt blocked next push", result, err)
	}
}
func TestSavedStartFailureAcknowledgesDeliveryWithRealStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := realStore(t)
	f.d.Runs = s
	f.d.Service = storeService{Store: s, Policy: f.runs}
	f.keeper.KeepFunc = func(context.Context, string, []sourcecontract.Binding) error {
		return permanentRetention("retention denied")
	}
	submitter := f.new(t)
	r := f.gate(t, submitter)
	result, err := submitter.Deliver(t.Context(), r)
	if err != nil || result.Outcomes[0].State != "failed" || !strings.Contains(strings.Join(result.Lines(), "\n"), "watch: https://oberth.ci/runs/r1") {
		t.Fatal(result, err)
	}
	row, err := s.Run(t.Context(), "r1")
	if err != nil || row.Status != "failed" || row.Detail != "retention denied" {
		t.Fatal(row, err)
	}
	result, err = submitter.Deliver(t.Context(), r)
	if err != nil || result.Outcomes[0].State != "failed" || f.creates.Load() != 0 {
		t.Fatal("durable failure requeued", result, err)
	}
}

func TestRejectedGrantCommandsSurviveDurableDeliveryReplay(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.d.Runs = realStore(t)
	f.d.Service = storeService{Store: f.d.Runs.(*store.Store), Policy: f.runs}
	w := workflow()
	w.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "kv/build"}
	w.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	f.setDocument(w)
	s := f.new(t)
	r := receive()
	for range 2 {
		result, err := s.Deliver(t.Context(), r)
		if err != nil {
			t.Fatal("saved rejection was not acknowledged", err)
		}
		text := strings.Join(result.Lines(), "\n")
		command := "  fix: oberth secret put --repo 'origin/team/widget' --file '.oberth/build.yaml' --step 'build' 'kv/build' < <value-file>"
		if !strings.Contains(text, command) || !strings.Contains(text, "refs/heads/topic") {
			t.Fatal("replay lost the authoritative grant fix", text)
		}
	}
	if f.creates.Load() != 0 {
		t.Fatal("rejected admission created workflow")
	}
}

type storeService struct {
	*store.Store
	Policy app.Service
}

func (s storeService) Accept(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	return s.AcceptBatch(ctx, b)
}
func (s storeService) Decide(ctx context.Context, u service.RefUpdate) (service.Decision, error) {
	if s.Policy != nil {
		return s.Policy.Decide(ctx, u)
	}
	return decision(u, ""), ctx.Err()
}

func TestGateRecordsAfterRepositoryArchived(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	db := realStore(t)
	s, authority := realServiceWiring(t, f, db)
	r := receive()
	r.Source = f.source
	attributeWorker(&r)
	if _, err := s.Gate(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := db.ArchiveRepository(t.Context(), stateport.Action{Actor: request().Actor.Identity}, 1); err != nil {
		t.Fatal(err)
	}
	authority.Err = errors.New("archived authority changed after gate")
	result, err := s.Deliver(t.Context(), r)
	if err != nil || len(result.Outcomes) != 1 || result.Outcomes[0].State != "started" {
		t.Fatal("sealed run refused after archive", result, err)
	}
	rows, err := db.AcceptedRuns(t.Context(), r.ID)
	if err != nil || len(rows) != 1 || rows[0].Origin.WorkflowUID == "" {
		t.Fatal("sealed workflow not bound", rows, err)
	}
}
