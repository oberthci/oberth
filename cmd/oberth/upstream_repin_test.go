package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/kubernetes"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

type listedUpstreams []model.Upstream

func (l listedUpstreams) ListUpstreams(context.Context) ([]model.Upstream, error) { return l, nil }

// --repin keeps an identical registration and refuses to silently change a
// different one under the same name.
func TestSameUpstreamRegisteredKeepsOnlyAnIdenticalRegistration(t *testing.T) {
	t.Parallel()
	spec := model.UpstreamSpec{Name: "forge", Kind: "ssh", BaseURL: "ssh://git@forge.example/~/acme"}
	database := listedUpstreams{{Name: "forge", Kind: "ssh", BaseURL: spec.BaseURL}}
	if registered, err := sameUpstreamRegistered(context.Background(), database, spec); err != nil || !registered {
		t.Fatalf("identical registration = %v, %v", registered, err)
	}
	if registered, err := sameUpstreamRegistered(context.Background(), listedUpstreams{}, spec); err != nil || registered {
		t.Fatalf("missing registration = %v, %v", registered, err)
	}
	moved := spec
	moved.BaseURL = "ssh://git@forge.example/acme"
	if _, err := sameUpstreamRegistered(context.Background(), database, moved); err == nil || !strings.Contains(err.Error(), "upstream remove") {
		t.Fatalf("changed registration error = %v", err)
	}
}

// --repin validates the registration before it rescans or writes anything:
// another URL or another key choice under the name is refused with the pins
// and the identity store untouched.
func TestUpstreamRepinRefusesAChangedRegistrationBeforeAnyPin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "oberth.sqlite")
	database, err := storetest.OpenAdminClient(ctx, databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.RegisterUpstream(ctx, "admin@test", model.UpstreamSpec{Name: "forge", Kind: "ssh", BaseURL: "ssh://git@forge.example/acme"})
	if closeErr := database.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	client := newBootstrapClient("ci", "oberth-upstream-key", "oberth-known-hosts")
	for name, test := range map[string]struct {
		arguments []string
		want      string
	}{
		"another URL":   {[]string{"forge", "ssh://git@other.example/acme"}, "upstream remove"},
		"dedicated key": {[]string{"--dedicated-key", "forge", "ssh://git@forge.example/acme"}, "without --dedicated-key"},
	} {
		t.Run(name, func(t *testing.T) {
			var operations []string
			dependencies := upstreamDependencies{
				input: strings.NewReader("yes\nyes\n"),
				mutationGate: func(_ context.Context, operation, _ string) error {
					operations = append(operations, operation)
					return nil
				},
				identityStore: fixtureStoreFactory(func() (kubernetes.Interface, error) { return client, nil }),
				scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
					t.Error("host keys were scanned before the registration was validated")
					return nil, nil
				},
				probe: func(context.Context, string, []byte, []byte) error {
					t.Error("the forge was probed before the registration was validated")
					return nil
				},
			}
			arguments := append([]string{
				"add", "--repin",
				"--database", databasePath,
				"--upstream-key", filepath.Join(directory, "projected", "id_ed25519"),
				"--known-hosts", filepath.Join(directory, "projected-hosts", "known_hosts"),
				"--namespace", "ci",
			}, test.arguments...)
			var output bytes.Buffer
			err := runUpstreamWithDependencies(ctx, arguments, &output, dependencies)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one naming %q", err, test.want)
			}
			for _, operation := range operations {
				if strings.HasPrefix(operation, "upstream.identity.") || operation == "upstream.register" {
					t.Fatalf("a refused re-pin reached %s: %v", operation, operations)
				}
			}
			if len(secretPatchNames(client)) != 0 {
				t.Fatalf("a refused re-pin wrote identities: %v", secretPatchNames(client))
			}
		})
	}
}

// A restored DB retains the registration while a fresh installation has no
// persisted deploy identity. Plain upstream add must generate it and keep the row.
func TestUpstreamAddAfterRestoreRegeneratesIdentityAndKeepsRegistration(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.sqlite")
	database, err := storetest.OpenAdminClient(ctx, databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	original, err := database.RegisterUpstream(ctx, "admin", model.UpstreamSpec{Name: "forge", Kind: "ssh", BaseURL: "ssh://git@forge.example/acme"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	client := newBootstrapClient("ci", "oberth-upstream-key", "oberth-known-hosts")
	hostKey := codebergSSHHostKey(t)
	dependencies := upstreamDependencies{input: strings.NewReader("yes\nyes\n"), mutationGate: allowTestMutation, identityStore: fixtureStoreFactory(func() (kubernetes.Interface, error) { return client, nil }), scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) { return []ssh.PublicKey{hostKey}, nil }, probe: func(_ context.Context, _ string, private, pins []byte) error {
		if _, err := ssh.ParsePrivateKey(private); err != nil {
			t.Error("regenerated identity is invalid")
		}
		if len(pins) == 0 {
			t.Error("host pins missing")
		}
		return nil
	}}
	var output bytes.Buffer
	args := []string{"add", "--database", databasePath, "--namespace", "ci", "--upstream-key", filepath.Join(root, "projected", "id_ed25519"), "--known-hosts", filepath.Join(root, "hosts", "known_hosts"), "forge", "ssh://git@forge.example/acme"}
	if err := runUpstreamWithDependencies(ctx, args, &output, dependencies); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "identical registration kept") || strings.Contains(output.String(), "PRIVATE KEY") {
		t.Fatal("restore registration guidance or secret boundary failed")
	}
	database, err = storetest.OpenAdminClient(ctx, databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows, err := database.ListUpstreams(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != original.ID {
		t.Fatal("restored registration was changed")
	}
	if len(secretPatchNames(client)) == 0 {
		t.Fatal("missing regenerated install identity")
	}
}
