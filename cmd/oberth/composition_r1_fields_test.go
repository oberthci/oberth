package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestCompositionR1MountedRefusalsNameFieldAndState(t *testing.T) {
	for _, field := range []string{"files.ssh_host_key", "files.ca_certificate", "files.ca_private_key", "files.admin_public_key", "files.admin_push_key", "files.secret_store_ca", "files.secret_store_token"} {
		for _, shape := range []string{"missing", "empty", "directory", "dangling"} {
			t.Run(field+"/"+shape, func(t *testing.T) {
				f := compositionRegressionFixture(t)
				fields := map[string]*string{"files.ssh_host_key": &f.config.Files.SSHHostKey, "files.ca_certificate": &f.config.Files.CACertificate, "files.ca_private_key": &f.config.Files.CAPrivateKey, "files.admin_public_key": &f.config.Files.AdminPublicKey, "files.admin_push_key": &f.config.Files.AdminPushKey, "files.secret_store_ca": &f.config.Files.SecretStoreCA, "files.secret_store_token": &f.config.Files.SecretStoreToken}
				*fields[field] = brokenStartupTarget(t, shape)
				if err := os.Remove(f.config.DatabaseFile); err != nil {
					t.Fatal(err)
				}
				assertStartupRefusalBeforeStorage(t, f, field)

			})
		}
	}
}
func TestCompositionR1RuntimeFieldsAreRequiredAndNamed(t *testing.T) {
	for _, field := range []string{"source_timeout_seconds", "admission_timeout_seconds", "source_client_burst", "default_branch"} {
		t.Run(field, func(t *testing.T) {
			var fields map[string]any
			if err := json.Unmarshal(validRuntime(), &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, field)
			raw, _ := json.Marshal(fields)
			_, err := decodeRuntime(raw)
			if err == nil || !strings.Contains(err.Error(), "runtime."+field) {
				t.Fatal("missing runtime field accepted or unnamed", err)
			}
		})
	}
}
func TestCompositionR1StartupDetailsAppearOnce(t *testing.T) {
	var diagnostic bytes.Buffer
	app := application{diagnostic: &diagnostic}
	failure := publicerr.New(publicerr.InvalidInput, "Mounted input is invalid.", "Fix the configured input.", errors.New("private cause marker"))
	failure.Detail = "files.ssh_host_key: invalid SSH private key\nfiles.forge_key: missing"
	if code := app.failedServer(failure); code != 1 {
		t.Fatal("wrong refusal exit", code)
	}
	for _, line := range strings.Split(failure.Detail, "\n") {
		if strings.Count(diagnostic.String(), line) != 1 {
			t.Fatal("detail repeated or missing", diagnostic.String())
		}
	}
	if strings.Contains(diagnostic.String(), "private cause marker") {
		t.Fatal("private cause exposed")
	}
}
func TestCompositionR1RemovedAssistSwitchIsRefused(t *testing.T) {
	raw := configCase(t, func(fields map[string]any) { fields["assist"].(map[string]any)["enabled"] = false })
	_, err := decodeConfig(raw)
	if err == nil || !strings.Contains(err.Error(), "assist.enabled: unknown field") {
		t.Fatal("unused switch accepted", err)
	}
}
