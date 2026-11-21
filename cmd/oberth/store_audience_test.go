package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStoreAudienceIsRequiredInConfig(t *testing.T) {
	for _, audience := range []any{nil, "", " ", " store-login", "store-login ", true} {
		data := configCase(t, func(c map[string]any) {
			store := c["secret_store"].(map[string]any)
			if audience == nil {
				delete(store, "audience")
			} else {
				store["audience"] = audience
			}
		})
		if _, err := decodeConfig(data); err == nil || !strings.Contains(err.Error(), "secret_store.audience") {
			t.Fatal("invalid store audience accepted or unnamed", audience, err)
		}
	}
}

func TestRuntimeCannotOverrideStoreAudience(t *testing.T) {
	for _, audience := range []string{"", "installation-store-login", "another-audience"} {
		data := configCase(t, func(c map[string]any) { c["runtime"].(map[string]any)["login_audience"] = audience })
		config, err := decodeConfig(data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = decodeRuntime(config.Runtime); err == nil || !strings.Contains(err.Error(), "login_audience") {
			t.Fatal("a second audience setting was accepted", audience, err)
		}
	}
	runtime, err := decodeRuntime(validRuntime())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(runtime)
	if err != nil || strings.Contains(string(raw), "login_audience") {
		t.Fatal("runtime still carries a separate audience", string(raw), err)
	}
}
