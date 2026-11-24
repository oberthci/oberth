package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestEffectiveSettingsExcludePrivateConfiguration(t *testing.T) {
	c := testComposition(t)
	taintStatusConfiguration(c)
	c.config.Installation = installationSettings{PodDefaults: true, BundledStore: true, LinkLocalFilter: true, PodSecurity: "baseline", HTTPSNodePort: 30443, SSHNodePort: 30022, Origins: map[string]string{"Pod defaults": "chosen"}}
	c.settings.Defaults.WorkflowDefaults = json.RawMessage(`{"env":[{"name":"PASSWORD","value":"PRIVATE-DEFAULT"}]}`)
	c.settings.SourcePolicy = json.RawMessage(`{"PRIVATE-SECRET-PATH":["PRIVATE-PRODUCER"]}`)
	a := serverAdministration{&serverOwners{c: c, settings: c.settings, cluster: clusterClients{Kube: statusControllerClient(c.config.ArgoNamespace, "4", "default")}}}
	status, err := a.Status(t.Context(), "system_status")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(status)
	if err != nil || bytes.Contains(raw, []byte("PRIVATE-")) {
		t.Fatal("status exposed private configuration", err)
	}
	if status.Settings == nil {
		t.Fatal("effective values are missing")
	}
	assertEffectiveSettings(t, *status.Settings)
	assertSettingsCLI(t, raw, *status.Settings)
}

func assertEffectiveSettings(t *testing.T, settings []apicontract.EffectiveSetting) {
	t.Helper()
	byName := map[string]apicontract.EffectiveSetting{}
	for _, row := range settings {
		if row.Source != "default" && row.Source != "chosen" && row.Source != "" {
			t.Fatal("missing origin", row)
		}
		byName[row.Name] = row
	}
	if byName["parallel runs"].Value != "4" || byName["parallel runs"].Source != "default" || byName["Pod defaults"].Source != "chosen" || byName["source pins"].Value != "1" {
		t.Fatal("effective values or explicit default choice lost", byName)
	}
	if _, exists := byName["scheduled backup"]; exists {
		t.Fatal("unmeasured backup schedule exposed")
	}
	for _, name := range []string{"version", "commit", "store mode", "store state", "server URL", "SSH port", "HTTPS node port", "Pod Security", "link-local egress filter", "Argo controller image", "Argo executor image", "source plugin image", "store image"} {
		if _, ok := byName[name]; !ok {
			t.Fatal("missing setting", name)
		}
	}
}

func assertSettingsCLI(t *testing.T, raw []byte, settings []apicontract.EffectiveSetting) {
	t.Helper()
	var out bytes.Buffer
	if err := (application{out: &out}).render(raw, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"SETTING", "VALUE", "SOURCE"} {
		if !strings.Contains(out.String(), column) {
			t.Fatal("settings table missing", out.String())
		}
	}
	for _, row := range settings {
		if !strings.Contains(out.String(), row.Name) || row.Value != "" && !strings.Contains(out.String(), row.Value) {
			t.Fatal("CLI differs from status", row)
		}
	}
}

func taintStatusConfiguration(c *composition) {
	// Taint every mounted-file field, private store configuration, the original
	// runtime object and opaque workflow policy, including future file fields.
	files := reflect.ValueOf(&c.config.Files).Elem()
	for i := range files.NumField() {
		files.Field(i).SetString("PRIVATE-MOUNT-" + files.Type().Field(i).Name)
	}
	store := reflect.ValueOf(&c.config.SecretStore).Elem()
	for i := range store.NumField() {
		store.Field(i).SetString("PRIVATE-STORE-" + store.Type().Field(i).Name)
	}
	c.config.Runtime = json.RawMessage(`{"environment":{"PASSWORD":"PRIVATE-RUNTIME"}}`)
}

func TestVersionMetadataDoesNotInventSettingsRows(t *testing.T) {
	raw, err := json.Marshal(apicontract.SystemStatus{Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = (application{out: &out}).render(raw, false, commonOptions{resultType: reflect.TypeFor[apicontract.SystemStatus]()}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "settings") || strings.Contains(out.String(), "SETTING") {
		t.Fatal("version metadata invented a settings section", out.String())
	}
}
