package main

import (
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestUpgradeFloorComesOnlyFromAdministratorState(t *testing.T) {
	protocol := stateport.Protocol{SchemaIdentity: "live-schema", AuditDomain: "live-audit"}
	for _, schema := range []int{1, 7} {
		bindings := fake.Bindings()
		bindings.AdminVersion.Handler = &fake.Handler[apicontract.AdminVersionArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{SchemaVersion: schema, Protocol: protocol}}
		app, _, _ := fixture(t, bindings)
		options, err := client.Resolve(client.Options{}, app.base, app.env)
		if err != nil {
			t.Fatal(err)
		}
		cfg := app.config
		cfg.Options = options
		remote, err := client.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		floor, err := liveInstallationState(t.Context(), remote)
		remote.Close()
		if err != nil || floor.Schema != schema || floor.Protocol != protocol {
			t.Fatal("floor used a profile, compiled schema or backup", floor, err)
		}
	}
}
func TestBareStepRefusesMissingInputsAndOldFlags(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	app.env = func(string) string { return "" }
	for _, args := range [][]string{{"secretstore", "exec", "--", "true"}, {"secretstore", "exec", "--config", "/tmp/unused", "--", "true"}, {"secretstore", "exec", "--paths", "registry/key", "--", "true"}} {
		diagnostic.Reset()
		if code := app.run(t.Context(), args); code == 0 {
			t.Fatal("step used profile or removed explicit input flags", args)
		}
	}
	if flags := installationFlags("secretstore exec"); flags.Lookup("token-file") != nil || flags.Lookup("scratch") != nil || flags.Lookup("ca-file") != nil {
		t.Fatal("bare step retained mounted-input override flags")
	}
}
