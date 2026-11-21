//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/identityname"
)

func TestComposedRevokeShowsStillStoredThenRetries(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	backend := f.secretBackend(t)
	f.prepareSecretCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.writeSecretThroughCLI(t)
	paths := f.seedRevokeTiers(t)
	f.checkDismissalCLI(t)
	app, out, diagnostic := f.revokeApplication(t)
	backend.Sealed = true
	if code := app.run(t.Context(), accessRevokeCLIArgs()); code != 1 || out.Len() != 0 {
		t.Fatal("sealed revoke claimed success", code, out.String())
	}
	expected := "Grant revoked; secret still stored.\nFix the store problem in detail, then repeat the revoke.\nSecret store is sealed.\nAn administrator runs oberth unseal, then retry.\n"
	if diagnostic.String() != expected {
		t.Error("store repair missing or repeated", diagnostic.String())
	}
	for _, path := range paths {
		if backend.Resources[path] == nil {
			t.Error("failed revoke lost retained value")
		}
	}
	f.checkDismissalCLI(t)
	diagnostic.Reset()
	backend.Sealed = false
	if code := app.run(t.Context(), accessRevokeCLIArgs()); code != 0 || !strings.Contains(out.String(), "revoked") || diagnostic.Len() != 0 {
		t.Fatal("revoke retry failed", code, out.String(), diagnostic.String())
	}
	for _, path := range paths {
		if backend.Resources[path] != nil {
			t.Error("retry left a tier account value")
		}
	}
	if strings.Contains(out.String()+diagnostic.String(), "fixture-secret") {
		t.Error("CLI exposed a secret")
	}
}
func (f *composedFixture) seedRevokeTiers(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		account, err := identityname.Name(identityname.Key{Repository: gittest.Repository, File: ".oberth/build.yaml", Step: "build"}, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.owners.c.storeWriter.Put(t.Context(), account, "registry/key", []byte("fixture-secret")); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, "/"+f.config.SecretStore.KVMount+"/steps/"+account+"/registry/key")
	}
	return paths
}
func (f *composedFixture) revokeApplication(t *testing.T) (application, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	app := application{out: &out, diagnostic: &diagnostic, env: func(key string) string {
		if key == "OBERTH_URL" {
			return f.config.Public.URL
		}
		return ""
	}, config: client.Config{Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport}}
	return app, &out, &diagnostic
}
func (f *composedFixture) checkDismissalCLI(t *testing.T) {
	t.Helper()
	app, out, diagnostic := f.revokeApplication(t)
	args := []string{"access", "dismiss", "--repo", gittest.Repository, "--workflow", ".oberth/build.yaml", "--step", "build", "--secret", "registry/key"}
	if code := app.run(t.Context(), args); code != 1 || out.Len() != 0 || diagnostic.String() != "Only pending requests can be dismissed.\nUse oberth access revoke for an approved or revoked grant.\n" {
		t.Error("approved or revoked dismissal was not refused", code, out.String(), diagnostic.String())
	}
}
