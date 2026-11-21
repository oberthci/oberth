package service_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunGetRequiresAuthenticatedOriginalAndCurrentCaller(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"authenticated", "blank original identity", "current method removed"} {
		t.Run(name, func(t *testing.T) { checkRunGetAuthentication(t, name) })
	}
}

func checkRunGetAuthentication(t *testing.T, name string) {
	t.Helper()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	a := caller()
	if name == "blank original identity" {
		a.Identity = " \t\n"
	}
	current := a
	if name == "current method removed" {
		current.AuthMethod = ""
	}
	f.store.Actors[a.Fingerprint] = current
	call, err := f.s.PrepareRunGet(t.Context(), apicontract.Caller{Actor: a}, apicontract.RunGetArgs{ID: r.ID})
	if name == "authenticated" {
		if err != nil || call == nil {
			t.Fatal("authenticated caller could not read its run", err)
		}
		return
	}
	requireCode(t, err, publicerr.Forbidden)
	if call != nil {
		t.Fatal("unauthenticated caller prepared a read")
	}
	if name == "blank original identity" && f.store.Calls()["Current"] != 0 {
		t.Fatal("blank caller reached the authority store")
	}
}
