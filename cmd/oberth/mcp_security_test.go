package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
)

type unreadMCP struct{ t *testing.T }

func (r unreadMCP) Read([]byte) (int, error) {
	r.t.Fatal("refused bridge read agent input")
	return 0, io.EOF
}
func TestMCPRefusesAdminAndImplicitPersonProfiles(t *testing.T) {
	for _, kind := range []string{"missing", "environment", "current", "explicit-admin", "bound-admin", "unknown"} {
		t.Run(kind, func(t *testing.T) { refuseMCPProfile(t, kind) })
	}
}
func refuseMCPProfile(t *testing.T, kind string) {
	t.Helper()
	app, diagnostic := pinnedMCPApplication(t)
	app.in = unreadMCP{t}
	app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("refused bridge used credentials")
		return nil, io.EOF
	})
	installRefusedProfile(t, app.base, kind)
	app.env = func(string) string {
		if kind == "environment" {
			return "admin"
		}
		return ""
	}
	app.git = func(context.Context, []string) (string, error) {
		if kind == "bound-admin" {
			return "admin", nil
		}
		return "", errors.New("not a bound checkout")
	}
	args := []string{"mcp"}
	if kind == "explicit-admin" || kind == "unknown" {
		args = append(args, "--profile", "admin")
	}
	if code := app.run(t.Context(), args); code != 1 || !strings.Contains(diagnostic.String(), "join <file>") {
		t.Fatalf("unsafe profile selection: %d %s", code, diagnostic.String())
	}
	assertMCPSelectionUnchanged(t, app.base, kind)
}

func assertMCPSelectionUnchanged(t *testing.T, base, kind string) {
	t.Helper()
	selected, err := os.Readlink(filepath.Join(base, "current"))
	if kind == "missing" || kind == "environment" {
		if !os.IsNotExist(err) {
			t.Fatal("bridge created a current profile")
		}
		return
	}
	if err != nil || selected != "admin" {
		t.Fatal("bridge modified person's current profile")
	}
}
func TestInstallRulingFlagsReachOwner(t *testing.T) {
	app, diagnostic := pinnedMCPApplication(t)
	called := false
	app.install = func(_ context.Context, request installationRequest, _ io.Reader, _ io.Writer) error {
		called = true
		for name, want := range map[string]string{"parallel": "3", "no-pod-defaults": "true", "no-egress": "true", "external-store": "https://store.example.test"} {
			if request.flags.Lookup(name).Value.String() != want {
				t.Fatalf("%s lost", name)
			}
		}
		supplied := 0
		request.flags.Visit(func(*flag.Flag) { supplied++ })
		if supplied != 4 {
			t.Fatal("ruling overrides missing")
		}
		return nil
	}
	if code := app.run(t.Context(), []string{"install", "--parallel", "3", "--no-pod-defaults", "--no-egress", "--external-store", "https://store.example.test"}); code != 0 || !called {
		t.Fatalf("install ruling flags failed: %d %s", code, diagnostic.String())
	}
	for _, name := range []string{"--parallel", "--no-pod-defaults", "--no-egress", "--external-store"} {
		if _, ok := commandCatalogue()["install"].Flags[name]; !ok {
			t.Fatalf("%s missing from command tree", name)
		}
	}
	if commandCatalogue()["mcp"].Flags["--profile"] != 1 {
		t.Fatal("explicit MCP profile missing from tree")
	}
}

func installRefusedProfile(t *testing.T, base, kind string) {
	t.Helper()
	dir := filepath.Join(base, "admin")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	profile := client.Profile{ServerURL: "https://example.test", AdminKey: filepath.Join(dir, "private.key"), AdminCert: filepath.Join(dir, "public.crt")}
	if kind == "unknown" {
		profile.AdminKey, profile.AdminCert = "", ""
	}
	data, _ := json.Marshal(profile)
	if err := client.WritePrivate(filepath.Join(dir, "installation.json"), data); err != nil {
		t.Fatal(err)
	}
	if kind != "missing" && kind != "environment" {
		if err := os.Symlink("admin", filepath.Join(base, "current")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMCPNoBindingIgnoresPersonUplink(t *testing.T) {
	app, diagnostic := pinnedMCPApplication(t)
	app.in = unreadMCP{t}
	if err := os.Symlink("pinned", filepath.Join(app.base, "current")); err != nil {
		t.Fatal(err)
	}
	app.git = func(context.Context, []string) (string, error) { return "", errors.New("unbound checkout") }
	if code := app.run(t.Context(), []string{"mcp"}); code != 1 || !strings.Contains(diagnostic.String(), "No uplink profile selected") {
		t.Fatalf("bridge fell back to the person's uplink: %d %s", code, diagnostic.String())
	}
}
func TestMCPEmptyExplicitProfileDoesNotFallBack(t *testing.T) {
	app, diagnostic := pinnedMCPApplication(t)
	app.in = unreadMCP{t}
	app.git = func(context.Context, []string) (string, error) {
		t.Fatal("explicit selection read checkout")
		return "pinned", nil
	}
	if code := app.run(t.Context(), []string{"mcp", "--profile", ""}); code != 1 || !strings.Contains(diagnostic.String(), "No uplink profile selected") {
		t.Fatalf("empty explicit profile fell back: %d %s", code, diagnostic.String())
	}
}
