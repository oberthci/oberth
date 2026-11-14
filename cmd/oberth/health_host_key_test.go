package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

func TestConfiguredHealthCarriesRealHostKeyToSystemStatus(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	health := configuredHealth(serveOptions{}, key)
	database, err := storetest.Open(t.Context(), filepath.Join(t.TempDir(), "state.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	svc, err := service.NewAPI(service.APIConfig{Health: health, Runs: database})
	if err != nil {
		t.Fatal(err)
	}
	value, err := svc.CallTool(t.Context(), api.Actor{Identity: "admin", Admin: true}, "system_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	status := value.(app.HealthStatus)
	want, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	if status.SSHHostKeyFingerprint != ssh.FingerprintSHA256(want) || status.SSHHostKey != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(want))) {
		t.Fatal("system_status lost configured server host key")
	}
}
