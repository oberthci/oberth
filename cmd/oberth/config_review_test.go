package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"net"
	"net/http"
)

func configCase(t *testing.T, change func(map[string]any)) []byte {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(validConfig(), &cfg); err != nil {
		t.Fatal(err)
	}
	change(cfg)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func TestConfigRefusalsNameKeyAndFile(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		detail string
	}{
		{"empty", nil, "byte 0"}, {"malformed", []byte(`{"version":`), "byte"},
		{"trailing", append(validConfig(), []byte(` {}`)...), "byte"},
		{"wrong type", configCase(t, func(c map[string]any) { c["public"].(map[string]any)["ssh_port"] = "2222" }), "public.ssh_port: expected integer"},
		{"missing", configCase(t, func(c map[string]any) { delete(c, "namespace") }), "namespace: required"},
		{"same namespace", configCase(t, func(c map[string]any) { c["pipeline_namespace"] = c["namespace"] }), "pipeline_namespace"},
		{"relative DB", configCase(t, func(c map[string]any) { c["database_file"] = "state.db" }), "database_file"},
		{"TTL", configCase(t, func(c map[string]any) { c["assist"].(map[string]any)["ttl_seconds"] = 0 }), "assist.ttl_seconds"},
		{"listen", configCase(t, func(c map[string]any) { c["listen"].(map[string]any)["https"] = "bad" }), "listen.https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.json")
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := readConfig(path)
			var e *publicerr.Error
			if !errors.As(err, &e) || !strings.Contains(e.Detail, path) || !strings.Contains(e.Detail, tc.detail) {
				t.Fatalf("unhelpful refusal: %v", err)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := readConfig(missing); err == nil || !strings.Contains(err.Error(), missing+": not found") {
		t.Fatal("missing config unnamed")
	}
	directory := t.TempDir()
	if _, err := readConfig(directory); err == nil || !strings.Contains(err.Error(), directory) {
		t.Fatal("unreadable path unnamed")
	}
	if fileReason(os.ErrPermission) != "permission denied" {
		t.Fatal("permission reason lost")
	}
}
func TestConfigDuplicatesNullAndBound(t *testing.T) {
	for _, data := range [][]byte{append([]byte(`{"version":1,`), validConfig()[1:]...), []byte(strings.Replace(string(validConfig()), `"ssh_port":2222`, `"ssh_port":22,"ssh_port":2222`, 1)), []byte(strings.Replace(string(validConfig()), `"runtime":{}`, `"runtime":{"x":1,"x":2}`, 1)), []byte(strings.Replace(string(validConfig()), `"runtime":{}`, `"runtime":{"items":[{"x":1,"x":2}]}`, 1))} {
		if _, err := decodeConfig(data); err == nil || !strings.Contains(err.Error(), "duplicate key") {
			t.Fatal("duplicate accepted")
		}
	}
	null := configCase(t, func(c map[string]any) { c["namespace"] = nil })
	if _, err := decodeConfig(null); err == nil || strings.Count(err.Error(), "namespace:") != 1 {
		t.Fatal("null key diagnosed twice")
	}
	for _, size := range []int{1 << 20, 1<<20 + 1} {
		data := append(validConfig(), []byte(strings.Repeat(" ", size-len(validConfig())))...)
		if _, err := decodeConfig(data); (err != nil) != (size > 1<<20) {
			t.Fatalf("config size %d: %v", size, err)
		}
	}
}
func TestAdminMountCasesNameField(t *testing.T) {
	for _, target := range []string{"admin.pub", "push.pub"} {
		for _, kind := range []string{"empty", "malformed", "options", "two keys", "directory", "trailing blank"} {
			t.Run(target+"/"+kind, func(t *testing.T) { adminMountCase(t, target, kind) })
		}
	}
}
func adminMountCase(t *testing.T, target, kind string) {
	t.Helper()
	dir := t.TempDir()
	a := writeAdminKey(t, dir, "admin.pub")
	b := writeAdminKey(t, dir, "push.pub")
	data := a
	if target == "push.pub" {
		data = b
	}
	path := filepath.Join(dir, target)
	if err := alterAdminMount(path, data, kind); err != nil {
		t.Fatal(err)
	}
	_, err := readAdmin(filepath.Join(dir, "admin.pub"), filepath.Join(dir, "push.pub"))
	field := "files.admin_public_key"
	if target == "push.pub" {
		field = "files.admin_push_key"
	}
	if kind == "trailing blank" {
		if err != nil {
			t.Fatal(err)
		}
	} else if err == nil || !strings.Contains(err.Error(), field) {
		t.Fatalf("field not diagnosed: %v", err)
	}
}
func alterAdminMount(path string, data []byte, kind string) error {
	switch kind {
	case "empty":
		data = nil
	case "malformed":
		data = []byte("not a key")
	case "options":
		data = append([]byte("restrict "), data...)
	case "two keys":
		data = append(append([]byte(nil), data...), data...)
	case "directory":
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.Mkdir(path, 0700)
	case "trailing blank":
		data = append(data, '\n', '\n')
	}
	return os.WriteFile(path, data, 0600)
}
func TestMountRefusalsBeforeStorageAndKeyMismatch(t *testing.T) {
	f := fixtureServer(t)
	_, otherKey := authority(t, f.clock.Now())
	keyPath := f.config.Files.CAPrivateKey
	if err := os.WriteFile(keyPath, otherKey, 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readMounts(f.config, f.deps)
	if err == nil || !strings.Contains(err.Error(), "files.ca_private_key") || !strings.Contains(err.Error(), "match") {
		t.Fatal("mismatch lacks key field")
	}
	if err = os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	_, _, err = readMounts(f.config, f.deps)
	if err == nil || !strings.Contains(err.Error(), "files.ca_private_key is missing") {
		t.Fatal("missing key field lost")
	}
	if f.c != nil {
		t.Fatal("storage opened before mount validation")
	}
}
func TestRequiredPortsAreDeterministic(t *testing.T) {
	c := &composition{}
	for range 100 {
		if err := c.checkPorts(); err == nil || !strings.Contains(err.Error(), "argowatch.Run") {
			t.Fatal("port order nondeterministic")
		}
	}
	c.cacheGC = func(context.Context) error { return nil }
	c.replayPending = func(context.Context) error { return nil }
	c.watch = func(context.Context, func()) error { return nil }
	c.ssh = func(context.Context, net.Listener) error { return nil }
	c.source = http.NotFoundHandler()
	c.enroll = &fake.Handler[model.InviteRedemption, model.InviteCertificate]{}
	c.streams = &fake.Streams{}
	if err := c.checkPorts(); err == nil || !strings.Contains(err.Error(), "service.InstallAdmin") {
		t.Fatal("admin installation port not checked")
	}
}

func TestServeAcceptsOnlyConfigOption(t *testing.T) {
	for _, args := range [][]string{nil, {"--unknown"}, {"server.json"}, {"--config="}, {"--config", "file", "extra"}} {
		var diagnostic strings.Builder
		app := application{diagnostic: &diagnostic}
		if code := app.serve(t.Context(), args); code != 2 {
			t.Fatalf("unknown serve option became a file: %v exit %d", args, code)
		}
	}
}
