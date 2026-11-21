package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func installationTestRemote(t *testing.T, bindings apicontract.Bindings) (application, *client.Client, *bytes.Buffer) {
	t.Helper()
	app, _, diagnostic := fixture(t, bindings)
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
	t.Cleanup(remote.Close)
	return app, remote, diagnostic
}

func TestUpgradeRefusesIncompleteLiveMetadata(t *testing.T) {
	for _, status := range []apicontract.SystemStatus{
		{SchemaVersion: 0, Protocol: stateport.DefaultProtocol()},
		{SchemaVersion: -1, Protocol: stateport.DefaultProtocol()},
		{SchemaVersion: 1},
		{SchemaVersion: 1, Protocol: stateport.Protocol{SchemaIdentity: "schema"}},
	} {
		bindings := fake.Bindings()
		bindings.AdminVersion.Handler = &fake.Handler[apicontract.AdminVersionArgs, apicontract.SystemStatus]{Value: status}
		_, remote, _ := installationTestRemote(t, bindings)
		_, err := liveInstallationState(t.Context(), remote)
		var refusal *publicerr.Error
		if !errors.As(err, &refusal) || refusal.Reason() != "Live installation metadata is missing or invalid." || refusal.Remedy() != "Use an administrator profile and check oberth system status." {
			t.Fatal("incomplete live floor was accepted or lost its remedy", status, err)
		}
	}
}

func TestBackupUsesConfiguredProtocol(t *testing.T) {
	flags := installationFlags("backup")
	request := installationRequest{name: "backup", flags: flags}
	options, err := installationStateOptions(request)
	if err != nil || options.Protocol != stateport.DefaultProtocol() || options.Schema != store.Schema {
		t.Fatal("fresh-install protocol default changed", options, err)
	}
	data := strings.ReplaceAll(string(validConfig()), "oberth-schema-v1", "custom-schema")
	data = strings.ReplaceAll(data, "oberth-audit-v2", "custom-audit")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := flags.Set("config", path); err != nil {
		t.Fatal(err)
	}
	options, err = installationStateOptions(request)
	want := stateport.Protocol{SchemaIdentity: "custom-schema", AuditDomain: "custom-audit"}
	if err != nil || options.Protocol != want || options.Schema != store.Schema {
		t.Fatal("backup used a compiled protocol instead of its configuration", options, err)
	}
}

func TestUpgradeAndRollbackAuditNeedRecordedReceipt(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		for _, state := range []string{"recorded", "", "pending"} {
			t.Run(fmt.Sprint(rollback, state), func(t *testing.T) { lifecycleAuditCase(t, rollback, state) })
		}
	}
}

func lifecycleAuditCase(t *testing.T, rollback bool, state string) {
	t.Helper()
	bindings := fake.Bindings()
	handler := &fake.Handler[apicontract.AdminAuditRecordArgs, apicontract.Receipt]{Value: apicontract.Receipt{State: state}}
	bindings.AdminAuditRecord.Handler = handler
	app, remote, diagnostic := installationTestRemote(t, bindings)
	s := installationSession{app: app, name: "chosen"}
	s.recordLifecycle(t.Context(), remote, installer.UpgradeCompletion{From: "v0.1.0", To: "v0.2.0", Revision: 3}, rollback)
	args, _, calls := handler.Observed()
	details := map[string]string{"from": "v0.1.0", "to": "v0.2.0", "revision": "3"}
	operation, action := "Upgrade", "upgrade.completed"
	if rollback {
		operation, action = "Rollback", "rollback.completed"
	}
	if calls != 1 || !reflect.DeepEqual(args, []apicontract.AdminAuditRecordArgs{{Action: action, Details: details}}) {
		t.Fatal("audit was replayed or changed", calls, args)
	}
	want := ""
	if state != "recorded" {
		want = operation + " succeeded; audit unconfirmed. Check: OBERTH_PROFILE='chosen' oberth audit\n"
	}
	if diagnostic.String() != want {
		t.Fatal("unconfirmed receipt reported as confirmed", diagnostic.String())
	}
}
