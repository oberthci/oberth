//go:build linux

// These tests exercise Linux memory-backed step secret delivery.

package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestSecretStepDeliversPrivatelyCleansAndPreservesExitStatus(t *testing.T) {
	f := newDeliveryFixture(t)
	backend := f.backend
	backend.Resources["/kv/steps/step-account/registry/key"] = map[string]any{"value": "Zml4dHVyZS1zZWNyZXQ="}
	server := httptest.NewTLSServer(f.handler(t))
	t.Cleanup(server.Close)
	cfg := f.o.c.config
	cfg.SecretStore.Address = server.URL
	request := storeOperatorRequest(t, cfg, server.Certificate().Raw)
	if err := os.WriteFile(request.text("token-file"), []byte(f.jwt), 0600); err != nil {
		t.Fatal(err)
	}
	_, token := projectedStepInputs(t, request)
	parent, err := os.MkdirTemp("/dev/shm", "c11-delivery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(parent) })
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, diagnostic := &bytes.Buffer{}, &bytes.Buffer{}
	delivery := runtimeconfig.SecretDelivery{BrokerAddress: server.URL, BrokerServerName: "127.0.0.1", BrokerCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})), Access: runtimeconfig.SecretStoreAccess(storeConfiguration(cfg))}
	configuration, _ := json.Marshal(delivery)
	env := map[string]string{runtimeconfig.SecretConfigEnv: string(configuration), runtimeconfig.SecretPathsEnv: `["registry/key"]`, runtimeconfig.SecretTokenEnv: token, runtimeconfig.SecretScratchEnv: parent}
	app := application{in: &bytes.Buffer{}, out: out, diagnostic: diagnostic, env: func(key string) string { return env[key] }}
	args := []string{"secretstore", "exec", "--", binary, "-test.run=^TestSecretDeliveryChildProcess$", "--", "c11-delivery-child"}
	if code := app.run(t.Context(), args); code != 23 || !backend.Revoked {
		t.Fatalf("step result/revocation: %d %v %s", code, backend.Revoked, diagnostic.String())
	}
	checkDeliveryCleanup(t, parent, out.String()+diagnostic.String())
	out.Reset()
	diagnostic.Reset()
	backend.Fail = "/kv/steps/step-account/registry/key"
	if code := app.run(t.Context(), args); code != 1 || out.Len() != 0 || !strings.Contains(diagnostic.String(), "Secret store is sealed.") {
		t.Fatalf("sealed store did not prevent command: %d %s", code, diagnostic.String())
	}
}

func projectedStepInputs(t *testing.T, request installationRequest) (string, string) {
	t.Helper()
	ca, token := request.text("ca-file")+".projected", request.text("token-file")+".projected"
	for link, target := range map[string]string{ca: request.text("ca-file"), token: request.text("token-file")} {
		if err := os.Symlink(filepath.Base(target), link); err != nil {
			t.Fatal(err)
		}
	}
	return ca, token
}

func checkDeliveryCleanup(t *testing.T, parent, output string) {
	t.Helper()
	if files, err := os.ReadDir(parent); err != nil || len(files) != 0 {
		t.Fatal("private delivery survived child exit", files, err)
	}
	if !strings.HasPrefix(output, parent+"/") || strings.Contains(output, "fixture-secret") {
		t.Fatal("step output exposed secret material or lacked its private directory")
	}
}

func deliveryHTTPServer(t *testing.T, backend *storefake.Store) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, err := backend.RoundTrip(r)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSecretDeliveryChildProcess(t *testing.T) {
	if !slices.Contains(os.Args, "c11-delivery-child") {
		return
	}
	dir := os.Getenv("OBERTH_SECRETS_DIR")
	data, err := os.ReadFile(filepath.Join(dir, "registry", "key"))
	if err != nil || string(data) != "fixture-secret" {
		t.Fatal("child did not receive its complete secret set")
	}
	info, err := os.Stat(filepath.Join(dir, "registry", "key"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("secret file was not private")
	}
	_, _ = io.WriteString(os.Stdout, dir)
	os.Exit(23)
}
