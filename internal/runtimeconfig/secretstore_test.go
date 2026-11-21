package runtimeconfig_test

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/runtimeconfig"
)

func TestSecretDeliveryWireContainsOnlyPublicConnectionFacts(t *testing.T) {
	raw, err := json.Marshal(runtimeconfig.SecretDelivery{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 {
		t.Fatal("secret delivery has extra authority")
	}
	for _, key := range []string{"address", "server_name", "ca_pem", "access"} {
		if _, ok := fields[key]; !ok {
			t.Fatal("missing public fact", key)
		}
	}
	if runtimeconfig.SecretConfigEnv != "OBERTH_SECRETSTORE_CONFIG" || runtimeconfig.SecretPathsEnv != "OBERTH_SECRETSTORE_PATHS" || runtimeconfig.SecretTokenEnv != "OBERTH_SECRETSTORE_TOKEN_FILE" || runtimeconfig.SecretScratchEnv != "OBERTH_SECRETSTORE_SCRATCH" {
		t.Fatal("step environment contract changed")
	}
}

func TestSecretStoreConfigurationRefusalIsBounded(t *testing.T) {
	reason, remedy := runtimeconfig.SecretStoreConfiguration()
	if len(reason) > 80 || len(remedy) > 120 {
		t.Fatal("configuration refusal exceeds its bound")
	}
}
