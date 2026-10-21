package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// The runs cursor is the oldest run's sequence, or its ID when a scoped
// uplink's server withholds sequences (#073).
func TestRunCursorFallsBackToTheRunID(t *testing.T) {
	t.Parallel()
	if runCursor(remoteRun{ID: "abc", QueueSequence: 7}) != "7" || runCursor(remoteRun{ID: "abc"}) != "abc" {
		t.Fatal("run cursor")
	}
	if !cursorMovedBack("9", "7") || !cursorMovedBack("7", "9") || cursorMovedBack("abc", "abc") || !cursorMovedBack("abc", "def") || !cursorMovedBack("abc", "") {
		t.Fatal("cursor progress")
	}
}

func TestScopeFlagIsRepeatableAndCommaSeparated(t *testing.T) {
	t.Parallel()
	var scope scopeFlag
	for _, value := range []string{"github, codeberg", "", "skipops"} {
		if err := scope.Set(value); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(scope, scopeFlag{"github", "codeberg", "skipops"}) || scopeLabel(nil) != "all" || scopeLabel([]string{"a", "b"}) != "a,b" {
		t.Fatalf("scope = %v", scope)
	}
}

// uplink add --scope, list, scope --set/--clear and rotate (#073).
func TestUplinkScopeLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	database := filepath.Join(directory, "oberth.sqlite")
	seed, err := store.Open(ctx, database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for name, base := range map[string]string{"github": "ssh://git@github.com/oberthci", "codeberg": "ssh://git@codeberg.org/cloudtaser"} {
		if _, err := seed.CreateUpstream(ctx, model.UpstreamSpec{Name: name, Kind: "ssh", BaseURL: base}); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	key, certificateKey := newTestUplinkKey(t)
	certificatePath := filepath.Join(directory, "tls.crt")
	writeTestCertificate(t, certificatePath, certificateKey.Public().(ed25519.PublicKey), certificateKey)
	deps := uplinkDependencies{mutationGate: allowTestMutation}
	run := func(input io.Reader, arguments ...string) (string, error) {
		var output bytes.Buffer
		err := runUplinkWithDependencies(ctx, arguments, input, &output, deps)
		return output.String(), err
	}
	rawKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))

	// A scope naming no upstream fails before any token is shown.
	if output, err := run(strings.NewReader(""), "add", "--database", database, "--tls-cert", certificatePath, "--scope", "gitlab", rawKey, "laptop@work"); err == nil || strings.Contains(output, "shown once") {
		t.Fatalf("unknown scope: %q, %v", output, err)
	}
	output, err := run(strings.NewReader(""), "add", "--database", database, "--tls-cert", certificatePath, "--scope", "github", rawKey, "laptop@work")
	if err != nil || !strings.Contains(output, "Uplink token for laptop@work (scope: github) (shown once)") {
		t.Fatalf("add: %q, %v", output, err)
	}
	list := func() string {
		t.Helper()
		listed, err := run(strings.NewReader(""), "list", "--database", database)
		if err != nil {
			t.Fatal(err)
		}
		return listed
	}
	if listed := list(); !strings.Contains(listed, "SCOPE") || !strings.Contains(listed, " github ") {
		t.Fatalf("list:\n%s", listed)
	}
	// Adding the same key again without --scope, as onboarding does, keeps it.
	if _, err := run(strings.NewReader(""), "add", "--database", database, "--tls-cert", certificatePath, rawKey, "laptop@work"); err != nil {
		t.Fatal(err)
	}
	if listed := list(); !strings.Contains(listed, " github ") || strings.Contains(listed, " all ") {
		t.Fatalf("re-add widened the scope:\n%s", listed)
	}
	if output, err := run(strings.NewReader(""), "scope", "--database", database, "--set", "github,codeberg", "laptop@work"); err != nil || !strings.Contains(output, "now reaches: codeberg,github") {
		t.Fatalf("scope --set: %q, %v", output, err)
	}

	// Rotation keeps the scope.
	newKey, _ := newTestUplinkKey(t)
	if _, err := run(bytes.NewReader(ssh.MarshalAuthorizedKey(newKey)), "rotate", "--database", database, "--tls-cert", certificatePath, "-", "laptop@work"); err != nil {
		t.Fatal(err)
	}
	if listed := list(); !strings.Contains(listed, ssh.FingerprintSHA256(newKey)) || !strings.Contains(listed, "codeberg,github") {
		t.Fatalf("rotate dropped the scope:\n%s", listed)
	}
	// Two keys of one identity with different scopes: rotation refuses to
	// guess, whichever key is listed first, including the new key itself.
	thirdKey, _ := newTestUplinkKey(t)
	if _, err := run(strings.NewReader(""), "add", "--database", database, "--tls-cert", certificatePath, "--scope", "github",
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(thirdKey))), "laptop@work"); err != nil {
		t.Fatal(err)
	}
	// Rotating to the already-registered key must still compare it.
	if _, err := run(bytes.NewReader(ssh.MarshalAuthorizedKey(thirdKey)), "rotate", "--database", database, "--tls-cert", certificatePath, "-", "laptop@work"); err == nil ||
		!strings.Contains(err.Error(), "different scopes") {
		t.Fatalf("rotate across mismatched scopes = %v", err)
	}
	if _, err := run(strings.NewReader(""), "remove", "--database", database, "--fingerprint", ssh.FingerprintSHA256(thirdKey), "laptop@work"); err != nil {
		t.Fatal(err)
	}
	if output, err := run(strings.NewReader(""), "scope", "--database", database, "--clear", "laptop@work"); err != nil || !strings.Contains(output, "now reaches: all") {
		t.Fatalf("scope --clear: %q, %v", output, err)
	}
	for _, arguments := range [][]string{
		{"scope", "--database", database, "laptop@work"},
		{"scope", "--database", database, "--clear", "--set", "github", "laptop@work"},
		{"scope", "--database", database, "--set", "github"},
	} {
		if _, err := run(strings.NewReader(""), arguments...); !errors.Is(err, errUsage) {
			t.Errorf("%v = %v, want usage", arguments, err)
		}
	}
}
