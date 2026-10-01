package main

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// TestBuildPerUpstreamSSHCommandUsesProjectedKeyFiles pins the serve-time
// contract: per-upstream identities are referenced in place on the Secret
// volume mount — no key bytes are read, copied, or written anywhere — the
// generated config maps each dedicated upstream host to its projected file
// with the shared key negated out of the fallback, rows without a dedicated
// key stay on the shared fallback, and a malformed key name from the
// database (treated as untrusted input) is skipped instead of being joined
// into a path.
func TestBuildPerUpstreamSSHCommandUsesProjectedKeyFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	keyDirectory := filepath.Join(directory, "upstream-key")
	if err := os.MkdirAll(keyDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	sharedKey := filepath.Join(keyDirectory, "id_ed25519")
	dedicatedKey := filepath.Join(keyDirectory, "id_ed25519-codeberg")
	knownHosts := filepath.Join(directory, "known_hosts")
	for _, path := range []string{sharedKey, dedicatedKey, knownHosts} {
		if err := os.WriteFile(path, []byte("placeholder"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	databasePath := filepath.Join(directory, "oberth.sqlite")
	database, err := store.OpenAdminClient(ctx, databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	for _, spec := range []model.UpstreamSpec{
		{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme", KeyName: "id_ed25519-codeberg"},
		{Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/octocat"},
		{Name: "hostile", Kind: "ssh", BaseURL: "ssh://git@hostile.example/intruder", KeyName: "../../../etc/passwd"},
		{Name: "unprojected", Kind: "ssh", BaseURL: "ssh://git@pending.example/pending-org", KeyName: "id_ed25519-unprojected"},
	} {
		if _, err := database.RegisterUpstream(ctx, "admin@test", spec); err != nil {
			t.Fatal(err)
		}
	}
	options := serveOptions{upstreamKey: sharedKey, knownHosts: knownHosts}
	configPath := filepath.Join(directory, "ssh-config")
	logger := log.New(io.Discard, "", 0)
	command, err := buildPerUpstreamSSHCommand(ctx, database, options, configPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	if command != "'ssh' '-F' '"+configPath+"'" {
		t.Fatalf("command = %q", command)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config := string(body)
	for _, required := range []string{
		"Host codeberg.org\n",
		`IdentityFile "` + dedicatedKey + `"`,
		"Host * !codeberg.org\n",
		`IdentityFile "` + sharedKey + `"`,
	} {
		if !strings.Contains(config, required) {
			t.Fatalf("config lacks %q:\n%s", required, config)
		}
	}
	for _, forbidden := range []string{"etc/passwd", "hostile.example", "pending.example", "placeholder"} {
		if strings.Contains(config, forbidden) {
			t.Fatalf("config must not contain %q:\n%s", forbidden, config)
		}
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestBuildPerUpstreamSSHCommandFallsBackToLegacySingleKey pins backward
// compatibility: with no dedicated keys registered, the command is the
// legacy inline single-key transport and no config file is written.
func TestBuildPerUpstreamSSHCommandFallsBackToLegacySingleKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	sharedKey := filepath.Join(directory, "id_ed25519")
	knownHosts := filepath.Join(directory, "known_hosts")
	databasePath := filepath.Join(directory, "oberth.sqlite")
	database, err := store.OpenAdminClient(ctx, databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.RegisterUpstream(ctx, "admin@test", model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/acme",
	}); err != nil {
		t.Fatal(err)
	}
	options := serveOptions{upstreamKey: sharedKey, knownHosts: knownHosts}
	configPath := filepath.Join(directory, "ssh-config")
	command, err := buildPerUpstreamSSHCommand(ctx, database, options, configPath, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "'-i' '"+sharedKey+"'") || !strings.Contains(command, "'-F' '/dev/null'") {
		t.Fatalf("legacy fallback command = %q", command)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("config file must not exist for the legacy path: %v", err)
	}
}
