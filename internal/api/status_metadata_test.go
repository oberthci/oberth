package api_test

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestAdminStatusRegistryDeclaresExactlySchemaAndProtocolMetadata(t *testing.T) {
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.AdminVersion.Handler = &fake.Handler[apicontract.AdminVersionArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{SchemaVersion: 7}}
	})
	w := request(f, "POST", "/api/admin/version", []byte(`{}`))
	assertSuccess(t, w)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["schema_version"]) != "7" || fields["settings"] != nil {
		t.Fatal("schema metadata missing")
	}
	b := fake.Bindings()
	registry, err := api.Builtin(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listTools(t, registry, admin()) {
		if tool.Name != "admin_version" {
			continue
		}
		properties := tool.OutputSchema["properties"].(apicontract.Schema)
		if len(properties) != 9 || properties["schema_version"].(apicontract.Schema)["type"] != "integer" || properties["protocol"].(apicontract.Schema)["type"] != "object" || properties["parts"].(apicontract.Schema)["type"] != "array" {
			t.Fatal("status metadata contract differs")
		}
		assertOptionalSettingsSchema(t, properties["settings"].(apicontract.Schema))
		assertLogStorageSchema(t, properties["log_storage"].(apicontract.Schema))
	}
}

func assertLogStorageSchema(t *testing.T, schema apicontract.Schema) {
	t.Helper()
	variants := schema["anyOf"].([]apicontract.Schema)
	if len(variants) != 2 || variants[0]["type"] != "object" || variants[1]["type"] != "null" {
		t.Fatal("optional log storage contract differs", variants)
	}
	properties := variants[0]["properties"].(apicontract.Schema)
	for _, name := range []string{"body", "index", "staging", "unknown", "open_spools"} {
		if properties[name].(apicontract.Schema)["type"] != "object" {
			t.Fatal("storage byte category missing", name)
		}
	}
}

func assertOptionalSettingsSchema(t *testing.T, schema apicontract.Schema) {
	t.Helper()
	settings := schema["anyOf"].([]apicontract.Schema)
	if len(settings) != 2 || settings[0]["type"] != "array" || settings[1]["type"] != "null" {
		t.Fatal("optional public settings contract differs", settings)
	}
}
