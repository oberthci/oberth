package service_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
)

type settingsAdministration struct {
	service.Administration
	settings []apicontract.EffectiveSetting
}

func (a settingsAdministration) Status(context.Context, string) (apicontract.SystemStatus, error) {
	return apicontract.SystemStatus{Settings: &a.settings}, nil
}

func TestRegistrationTotalsAreOnlyVisibleToAdministrators(t *testing.T) {
	p := people(t)
	p.f.c.People.Administration = settingsAdministration{Administration: p.f.c.People.Administration, settings: []apicontract.EffectiveSetting{
		{Name: "server URL", Value: "https://ci.example", Source: "chosen"},
		{Name: "registered upstreams", Value: "19"},
		{Name: "registered repositories", Value: "23"},
	}}
	p.f.restart(t)
	for _, caller := range []apicontract.Caller{workerCaller(), adminCaller(), workerCaller()} {
		call, err := p.f.s.PrepareSystemStatus(t.Context(), caller, apicontract.SystemStatusArgs{})
		status, err := execute(t.Context(), call, err)
		if err != nil || status.Settings == nil {
			t.Fatal("authenticated caller lost settings", err)
		}
		want := 1
		if apicontract.Admin.Allows(caller.Actor) {
			want = 3
		}
		if len(*status.Settings) != want || (*status.Settings)[0].Name != "server URL" {
			t.Fatal("registration total visibility differs", caller.Actor, *status.Settings)
		}
	}
}
