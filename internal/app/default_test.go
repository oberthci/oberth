package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

var _ service.Source = (*app.Submitter)(nil)

func TestSourceDefaultIsFreshForActualServicePolicy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	branch := "refs/heads/topic"
	var sourceErr error
	f.source.Export = func(context.Context, string, sourcecontract.Ref) (sourcecontract.Export, error) {
		return sourcecontract.Export{DefaultBranch: sourcecontract.Ref{Name: branch}}, sourceErr
	}
	wiring := f.new(t)
	svc, e := sourcePolicyService(t, wiring)
	if _, err := svc.Decide(t.Context(), e); err == nil {
		t.Fatal("agent moved current default")
	}
	branch = "refs/heads/new-default"
	d, err := svc.Decide(t.Context(), e)
	if err != nil || d.Action != "run" {
		t.Fatal("service retained stale source default", d, err)
	}
	sourceErr = errors.New("source metadata unavailable")
	if _, err := svc.Decide(t.Context(), e); !errors.Is(err, sourceErr) {
		t.Fatal("source outage became guessed default", err)
	}
}
func TestDefaultBranchHasNoCatalogueOrMainFallback(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	outage := errors.New("default head cannot be read")
	f.source.Export = func(context.Context, string, sourcecontract.Ref) (sourcecontract.Export, error) {
		return sourcecontract.Export{}, outage
	}
	branch, err := f.new(t).DefaultBranch(t.Context(), request().Repository)
	if branch != "" || !errors.Is(err, outage) {
		t.Fatal(branch, err)
	}
}
func sourcePolicyService(t *testing.T, wiring *app.Submitter) (*service.Service, service.RefUpdate) {
	t.Helper()
	db := realStore(t)
	ports := newServicePorts(t, db, wiring)
	ports.Authority.(*testAuthority).agent = true
	a := request().Actor
	a.Fingerprint = "worker"
	s, err := service.New(t.Context(), ports)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	r := request()
	e := service.RefUpdate{ID: "source-default", Repo: r.Repository, Ref: r.Ref.Name, ObjectSHA: r.Ref.ObjectSHA, CommitSHA: r.Ref.CommitSHA, Actor: a, Run: true}
	return s, e
}
