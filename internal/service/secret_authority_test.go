package service_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestSecretPutRechecksCallerBeforeAuditAndWrite(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"unchanged", "administrator removed", "scope narrowed"} {
		t.Run(name, func(t *testing.T) {
			p := people(t)
			q := secretTarget()
			approveSecret(t, p, q)
			writes := 0
			p.f.c.People.SecretWriter = secretWriterFunc(func(context.Context, string, string, []byte) error { writes++; return nil })
			p.f.restart(t)
			ctx := apicontract.WithSecretValue(t.Context(), []byte("private-body-marker"))
			call, err := p.f.s.PrepareSecretPut(ctx, adminCaller(), q)
			if err != nil {
				t.Fatal(err)
			}
			a := admin()
			switch name {
			case "administrator removed":
				a.Admin = false
			case "scope narrowed":
				a.Scope = []string{"origin"}
			}
			p.f.store.Actors[a.Fingerprint] = a
			_, err = call(ctx)
			wantWrites := 0
			if name == "unchanged" {
				wantWrites = 1
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireCode(t, err, publicerr.Forbidden)
			}
			rows, err := p.state.AdministrativeAudit(ctx, nil, stateport.AdministrativeFilter{Action: "secret.put.requested", Limit: 10})
			if err != nil || len(rows) != wantWrites || writes != wantWrites {
				t.Fatal("live authority did not govern audit and write", len(rows), writes, err)
			}
		})
	}
}
