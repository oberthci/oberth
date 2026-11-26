package service_test

import (
	"errors"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"testing"
)

func TestAdministrativeScopeTable(t *testing.T) {
	t.Parallel()
	f := setup(t)
	bindings := f.s.Bindings()
	n := 0
	for _, d := range apicontract.Definitions(bindings) {
		if !d.Bound {
			continue
		}
		n++
		need, ok := apicontract.Required(d.Name)
		if !ok {
			t.Fatal(d.Name)
		}
		for _, a := range []actor.Actor{admin(), func() actor.Actor { a := admin(); a.Scope = []string{"origin"}; return a }(), caller(), {}} {
			if need.Allows(a) && a.Identity != "" {
				continue
			}
			f.store.Actors[a.Fingerprint] = a
			_, err := d.Prepare(t.Context(), apicontract.Caller{Actor: a}, d.NewInput())
			if err == nil {
				t.Fatal("authority bypass", d.Name, a)
			}
			var refused *publicerr.Error
			if !errors.As(err, &refused) || refused.Code() != publicerr.Forbidden {
				t.Fatal("authority was not checked before inputs/dependencies", d.Name, err)
			}
		}
	}
	if n != 75 {
		t.Fatal("handler count", n)
	}
}
