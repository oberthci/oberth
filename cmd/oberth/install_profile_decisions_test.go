package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/client"
)

func TestInstallationRefusesUplinkOwner(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.base = t.TempDir()
	if err := os.Chmod(app.base, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(app.base, "selected")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(client.Profile{Uplink: true, ServerURL: "https://ci.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	app.install = func(_ context.Context, r installationRequest, _ io.Reader, _ io.Writer) error {
		_, err := app.openInstallation(r)
		return err
	}
	want := "An uplink profile cannot own an installation.\nChoose an administrator installation profile with --profile.\n"
	for _, command := range []string{"install", "upgrade", "rollback", "backup", "unseal"} {
		diagnostic.Reset()
		if code := app.run(t.Context(), []string{command, "--profile=selected"}); code != 1 || out.Len() != 0 || diagnostic.String() != want {
			t.Fatal(command, code, out.String(), diagnostic.String())
		}
	}
}

func TestOnlyFreshInstallAcceptsMissingProfile(t *testing.T) {
	for _, command := range []string{"install", "upgrade", "rollback", "backup", "unseal"} {
		t.Run(command, func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			app.base = t.TempDir()
			if err := os.Chmod(app.base, 0700); err != nil {
				t.Fatal(err)
			}
			app.install = func(_ context.Context, r installationRequest, _ io.Reader, _ io.Writer) error {
				_, err := app.openInstallation(r)
				return err
			}
			want := 1
			if command == "install" {
				want = 0
			}
			if code := app.run(t.Context(), []string{command, "--profile=missing"}); code != want || out.Len() != 0 {
				t.Fatal(command, code, out.String(), diagnostic.String())
			}
			entries, err := os.ReadDir(app.base)
			if err != nil || len(entries) != 0 {
				t.Fatal("profile lookup created local state", entries, err)
			}
		})
	}
}
