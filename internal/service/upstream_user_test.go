package service_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
)

func TestUpstreamAddressWithTheForgesOwnUserReachesHandshake(t *testing.T) {
	t.Parallel()
	const base = "ssh://oberth@192.168.86.247/home/oberth/repos"
	p, _ := confirmationFixture(t)
	var reached atomic.Bool
	p.f.c.People.Administration.(*testAdministration).Prepare = func(_ context.Context, q apicontract.UpstreamAddArgs) (service.PreparedUpstream, error) {
		reached.Store(q.BaseURL == base)
		return service.PreparedUpstream{}, errors.New("handshake stops the test")
	}
	p.f.restart(t)
	call, err := p.f.s.PrepareUpstreamAdd(t.Context(), adminCaller(), apicontract.UpstreamAddArgs{Name: "plain", BaseURL: base, CheckRepo: "repos/oberth-smoke"})
	_, _ = execute(t.Context(), call, err)
	if !reached.Load() {
		t.Fatal("the forge's own SSH user or namespace was refused", err)
	}
}
