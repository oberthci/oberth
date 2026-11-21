package client

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBridgeRefusesAdministratorBeforeCredentialsOrIO(t *testing.T) {
	for _, kind := range []string{"administrator", "unknown", "marked-admin"} {
		t.Run(kind, func(t *testing.T) { bridgeRefusedProfile(t, kind) })
	}
}
func bridgeRefusedProfile(t *testing.T, kind string) {
	t.Helper()
	cfg := bridgeProfile(t, roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("administrator credentials reached network")
		return nil, io.EOF
	}))
	dir := filepath.Join(cfg.Base, cfg.Profile)
	profile := Profile{ServerURL: "https://example.test", Uplink: kind == "marked-admin"}
	if kind != "unknown" {
		profile.AdminKey = filepath.Join(dir, "missing.key")
		profile.AdminCert = filepath.Join(dir, "missing.crt")
	}
	data, _ := json.Marshal(profile)
	if err := WritePrivate(filepath.Join(dir, "installation.json"), data); err != nil {
		t.Fatal(err)
	}
	if err := Bridge(t.Context(), strings.NewReader(""), io.Discard, cfg); err == nil || !strings.Contains(err.Error(), "join <file>") {
		t.Fatalf("administrator/unknown profile accepted: %v", err)
	}
}
func TestBridgeDoesNotUseCurrentProfileImplicitly(t *testing.T) {
	cfg := bridgeProfile(t, roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("implicit current profile reached network")
		return nil, io.EOF
	}))
	if err := os.Symlink(cfg.Profile, filepath.Join(cfg.Base, "current")); err != nil {
		t.Fatal(err)
	}
	cfg.Profile = ""
	if err := Bridge(t.Context(), strings.NewReader(""), io.Discard, cfg); err == nil {
		t.Fatal("current profile implicitly selected")
	}
}
