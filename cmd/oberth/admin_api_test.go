package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/api"
)

func TestServerAdministrationUsesProjectedIdentity(t *testing.T) {
	t.Setenv("VAULT_ADDR", "https://openbao.invalid:8200")
	t.Setenv("VAULT_CACERT", "")
	t.Setenv("OBERTH_VAULT_ROLE", "oberth")
	t.Setenv("OBERTH_VAULT_INSECURE_DEV_HTTP", "")
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	privatePath := filepath.Join(root, "id_ed25519")
	hostsPath := filepath.Join(root, "known_hosts")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostsPath, append([]byte("source.invalid "), ssh.MarshalAuthorizedKey(signer.PublicKey())...), 0600); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	adapters := serverAdministration(serveOptions{namespace: "oberth", upstreamKey: privatePath, knownHosts: hostsPath}, nil, nil, func(context.Context) error { mutations++; return nil })
	identity, err := adapters.EnsureUpstream(t.Context(), api.UpstreamAddRequest{Name: "north", BaseURL: "ssh://git@source.invalid/team", NoWait: true})
	if err != nil || !identity.Pending || identity.Generated || identity.Fingerprint == "" || mutations != 0 {
		t.Fatal(identity.Pending, identity.Generated, identity.Fingerprint, mutations, err)
	}
}
