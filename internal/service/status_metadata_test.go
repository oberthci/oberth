package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

type stateMetadataFunc func(context.Context) (int, stateport.Protocol, error)

func (f stateMetadataFunc) StateMetadata(ctx context.Context) (int, stateport.Protocol, error) {
	return f(ctx)
}

func TestAdministratorStatusUsesLiveStoreMetadata(t *testing.T) {
	t.Parallel()
	p := people(t)
	for _, tool := range []string{"admin_version", "system_status"} {
		t.Run(tool, func(t *testing.T) {
			for _, d := range apicontract.Definitions(p.f.s.Bindings()) {
				if d.Name != tool {
					continue
				}
				call, err := d.Prepare(t.Context(), adminCaller(), d.NewInput())
				value, err := execute(t.Context(), call, err)
				if err != nil {
					t.Fatal(err)
				}
				status := value.(apicontract.SystemStatus)
				if status.SchemaVersion != store.Schema || status.Protocol != stateport.DefaultProtocol() {
					t.Fatal("status omitted live metadata")
				}
			}
		})
	}
}

func TestStatusDoesNotInventMetadataOrExposeItToNonAdministrator(t *testing.T) {
	t.Parallel()
	p := people(t)
	calls := 0
	p.f.c.People.StateMetadata = stateMetadataFunc(func(context.Context) (int, stateport.Protocol, error) {
		calls++
		return 19, stateport.Protocol{SchemaIdentity: "live-schema", AuditDomain: "live-audit"}, nil
	})
	p.f.restart(t)
	call, err := p.f.s.PrepareAdminVersion(t.Context(), adminCaller(), apicontract.AdminVersionArgs{})
	status, err := execute(t.Context(), call, err)
	if err != nil || status.SchemaVersion != 19 || status.Protocol.SchemaIdentity != "live-schema" {
		t.Fatal("compiled metadata replaced live facts", err)
	}
	call, err = p.f.s.PrepareSystemStatus(t.Context(), workerCaller(), apicontract.SystemStatusArgs{})
	status, err = execute(t.Context(), call, err)
	if err != nil || calls != 1 || status.SchemaVersion != 0 || status.Protocol != (stateport.Protocol{}) {
		t.Fatal("metadata exposed to nonadministrator", err)
	}
}

func TestAdministratorStatusRefusesUnavailableMetadata(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-failure", true: "missing-port"}[missing], func(t *testing.T) {
			p := people(t)
			p.f.c.People.StateMetadata = stateMetadataFunc(func(context.Context) (int, stateport.Protocol, error) {
				return 0, stateport.Protocol{}, errors.New("read unavailable")
			})
			if missing {
				p.f.c.People.StateMetadata = nil
			}
			p.f.restart(t)
			call, err := p.f.s.PrepareAdminVersion(t.Context(), adminCaller(), apicontract.AdminVersionArgs{})
			status, err := execute(t.Context(), call, err)
			if err == nil || status.SchemaVersion != 0 || status.Protocol != (stateport.Protocol{}) {
				t.Fatal("missing live facts reported success")
			}
		})
	}
}
