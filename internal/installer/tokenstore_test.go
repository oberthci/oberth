package installer

import (
	"context"
	"errors"
	"os/user"
	"runtime"
	"strings"
	"testing"
)

// A credential the installer holds and then leaves as a copy-paste is the one
// that gets lost. It goes to the host's store at the moment it is minted.
func TestACredentialIsStoredRatherThanPrinted(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Keychain path is macOS only")
	}
	var argv []string
	var stdin string
	deps := Deps{RunCommand: func(_ context.Context, in []byte, name string, args ...string) ([]byte, error) {
		argv = append([]string{name}, args...)
		stdin = string(in)
		return nil, nil
	}}

	if err := storeSecret(context.Background(), deps, openBaoRootTokenLocation, "s.roottoken"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "add-generic-password") || !strings.Contains(joined, "-U") {
		t.Fatalf("wrong command: %s", joined)
	}
	// -U, or a reinstall adds a second item under one service and the read
	// answers with whichever came first. The account is resolved from the user
	// database rather than $USER, which is caller-controlled and about to
	// become a command argument.
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joined, "-a "+account.Username) {
		t.Fatalf("the account is not named, so the read may match another item: %s", joined)
	}
	// Inline, because `security -w` with no value reads the controlling
	// terminal rather than stdin: piping to it stops the install at a Keychain
	// prompt asking for a password the install already holds.
	if !strings.HasSuffix(joined, "-w s.roottoken") {
		t.Fatalf("the value is not supplied to the command: %s", joined)
	}
	if stdin != "" {
		t.Fatalf("stdin was %q, want nothing: security does not read it", stdin)
	}
}

func TestAFailedStoreIsReportedNotFatal(t *testing.T) {
	deps := Deps{RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
		return []byte("keychain locked"), errors.New("exit status 1")
	}}
	if err := storeSecret(context.Background(), deps, openBaoRootTokenLocation, "s.roottoken"); err == nil {
		t.Fatal("a failed store reported success")
	}
}

func TestAnEmptyValueIsNotStored(t *testing.T) {
	called := false
	deps := Deps{RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}}
	if err := storeSecret(context.Background(), deps, openBaoRootTokenLocation, "   "); err == nil {
		t.Fatal("an empty value was accepted")
	}
	if called {
		t.Fatal("the secret store was invoked with nothing to store")
	}
}

// The OpenBao root token and unseal key are the credentials that cannot be
// reissued: an initialized store whose unseal key is gone is permanently
// sealed. They go to the same store the bearer token goes to, under their own
// service names, so `oberth unseal` can find the key later.
func TestOpenBaoCredentialsGoToTheirOwnServices(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Keychain path is macOS only")
	}
	var argv []string
	deps := Deps{RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		argv = append(argv, strings.Join(append([]string{name}, args...), " "))
		return nil, nil
	}}

	if err := storeSecret(context.Background(), deps, openBaoRootTokenLocation, "s.rootvalue"); err != nil {
		t.Fatal(err)
	}
	if err := storeSecret(context.Background(), deps, openBaoUnsealKeyLocation, "unsealvalue"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\n")
	for _, want := range []string{"-s oberth-openbao-root", "-s oberth-openbao-unseal", "-U"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

// A hand-made Keychain entry carries whatever account the operator typed.
// Refusing to find it would make the installer's own convention the only one
// that works, on a machine where the entry already exists.
func TestTheKeychainReadFallsBackToAServiceOnlyLookup(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Keychain path is macOS only")
	}
	var attempts []string
	deps := Deps{RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		joined := strings.Join(append([]string{name}, args...), " ")
		attempts = append(attempts, joined)
		if strings.Contains(joined, "-a ") {
			return nil, errors.New("exit status 44")
		}
		return []byte("unsealvalue\n"), nil
	}}

	got, err := readStoredSecret(context.Background(), deps, openBaoUnsealKeyLocation)
	if err != nil {
		t.Fatal(err)
	}
	if got != "unsealvalue" {
		t.Fatalf("read %q, want the stored key", got)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected an account-scoped read then a service-only one, got:\n%s", strings.Join(attempts, "\n"))
	}
}

// A missing entry is a distinguishable condition, because the command that
// reads it is about to tell an operator what to do about it.
func TestAMissingEntryIsReportedAsNotStored(t *testing.T) {
	deps := Deps{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			return nil, errors.New("exit status 44")
		},
	}
	_, err := readStoredSecret(context.Background(), deps, openBaoUnsealKeyLocation)
	if runtime.GOOS == "darwin" {
		if !errors.Is(err, errSecretNotStored) {
			t.Fatalf("err = %v, want errSecretNotStored", err)
		}
		return
	}
	if !errors.Is(err, errNoSecretStore) {
		t.Fatalf("err = %v, want errNoSecretStore", err)
	}
}
