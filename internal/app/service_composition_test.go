package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

var _ app.Service = (*service.Service)(nil)

func TestRealServiceRecordsMixedPushAfterAuthorityChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	db := realStore(t)
	s, authority := realServiceWiring(t, f, db)
	first := receive()
	first.Source = f.source
	attributeWorker(&first)
	if _, err := s.Gate(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deliver(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	mixed := mixedReceive("mixed", "r2")
	attributeWorker(&mixed)
	if _, err := s.Gate(t.Context(), mixed); err != nil {
		t.Fatal(err)
	}
	before := authority.Calls()["Current"]
	authority.Err = errors.New("authority changed after seal")
	result, err := s.Deliver(t.Context(), mixed)
	assertMixedResult(t, result, err)
	assertRecordedDeletion(t, f, first.ID, func(ctx context.Context, _ string) (model.Publication, error) {
		return db.Publication(ctx, "deletion:"+mixed.ID+":"+request().Ref.Name)
	})
	if authority.Calls()["Current"] != before {
		t.Fatal("real service refreshed authority after seal")
	}
	if _, err := s.Deliver(t.Context(), mixed); err != nil {
		t.Fatal(err)
	}
	if f.prepares.Load() != 2 || f.creates.Load() != 2 {
		t.Fatal("real recording repeated prepare or create")
	}
}
func attributeWorker(r *app.Receive) {
	for i := range r.Updates {
		r.Updates[i].Request.Actor.Fingerprint = "worker"
	}
}
func realServiceWiring(t *testing.T, f *fixture, db serviceStore) (*app.Submitter, *testAuthority) {
	t.Helper()
	forwarding := &serviceForward{}
	f.d.Runs, f.d.Service = db, forwarding
	wiring := f.new(t)
	ports := newServicePorts(t, db, wiring)
	authority := ports.Authority.(*testAuthority)
	svc, err := service.New(t.Context(), ports)
	if err != nil {
		t.Fatal(err)
	}
	forwarding.Service = svc
	t.Cleanup(func() {
		if err := svc.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return wiring, authority
}

// Both constructors need the other's port; this fixture forwards unchanged values.
type serviceForward struct{ *service.Service }

type serviceStore interface {
	app.Runs
	service.RunStore
	service.EventStore
}
