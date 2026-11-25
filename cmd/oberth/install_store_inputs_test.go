package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/secretstore"
)

func TestProjectedStoreTokenBounds(t *testing.T) {
	for _, size := range []int{0, 1, 64 << 10, (64 << 10) + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			projectedStoreTokenCase(t, size)
		})
	}
}

func projectedStoreTokenCase(t *testing.T, size int) {
	t.Helper()
	dir := t.TempDir()
	data := bytes.Repeat([]byte("x"), size)
	if err := os.WriteFile(filepath.Join(dir, "value"), data, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "projected")
	if err := os.Symlink("value", path); err != nil {
		t.Fatal(err)
	}
	got, err := projectedStoreToken(path)
	if size == 1 || size == 64<<10 {
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("valid projection was refused or truncated", len(got), err)
		}
		return
	}
	want := refusalMessage{Reason: "Secret-store input file is missing or invalid.", Remedy: "Check the named file, its size and its mount, then retry.", Context: []string{"Input: --token-file", "File: " + path}}
	if got != nil || !reflect.DeepEqual(errorRefusal(err), want) {
		t.Fatal("invalid projection was accepted or lost its file remedy", len(got), err)
	}
}

func TestSecretStepEnvironmentContract(t *testing.T) {
	cfg, err := decodeConfig(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	delivery := runtimeconfig.SecretDelivery{BrokerAddress: "https://delivery.example", BrokerServerName: "delivery.example", BrokerCAPEM: "mounted-ca", Access: runtimeconfig.SecretStoreAccess(storeConfiguration(cfg))}
	raw, err := json.Marshal(delivery)
	if err != nil {
		t.Fatal(err)
	}
	limit := string(raw) + strings.Repeat(" ", (1<<20)-len(raw))
	wantInputs := secretStepInputs{connection: secretstore.Connection{URL: delivery.BrokerAddress, ServerName: delivery.BrokerServerName, CA: []byte(delivery.BrokerCAPEM)}, paths: []string{"registry/key"}, token: "/mounted/token", scratch: "/mounted/scratch"}
	for _, row := range []struct{ name, key, value string }{
		{"valid", "", ""},
		{"exact limit", runtimeconfig.SecretConfigEnv, limit},
		{"over limit", runtimeconfig.SecretConfigEnv, limit + " "},
		{"unknown field", runtimeconfig.SecretConfigEnv, strings.TrimSuffix(string(raw), "}") + `,"unknown":true}`},
		{"empty paths", runtimeconfig.SecretPathsEnv, `[]`},
		{"relative token", runtimeconfig.SecretTokenEnv, "token"},
		{"relative scratch", runtimeconfig.SecretScratchEnv, "scratch"},
	} {
		t.Run(row.name, func(t *testing.T) {
			env := map[string]string{runtimeconfig.SecretConfigEnv: string(raw), runtimeconfig.SecretPathsEnv: `["registry/key"]`, runtimeconfig.SecretTokenEnv: "/mounted/token", runtimeconfig.SecretScratchEnv: "/mounted/scratch"}
			if row.key != "" {
				env[row.key] = row.value
			}
			app := application{env: func(key string) string { return env[key] }}
			inputs, err := app.secretStepEnvironment()
			if row.name == "valid" || row.name == "exact limit" {
				if err != nil || !reflect.DeepEqual(inputs, wantInputs) {
					t.Fatal("admitted step inputs changed", inputs, err)
				}
				return
			}
			want := refusalMessage{Reason: "Step secret-store inputs are missing or invalid.", Remedy: "Run this command in a step with an approved secret grant.", Context: []string{"Input: " + row.key}}
			if !reflect.DeepEqual(errorRefusal(err), want) {
				t.Fatal("invalid environment was accepted or lost its named input", err)
			}
		})
	}
}
