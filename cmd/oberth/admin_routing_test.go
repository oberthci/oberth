package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAccessRoutesSubcommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no subcommand", args: nil, wantErr: "access list|allow|revoke"},
		{name: "unknown subcommand", args: []string{"unknown"}, wantErr: "access list"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := runAccess(context.Background(), test.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("runAccess(%q) = %v, want %q", test.args, err, test.wantErr)
			}
		})
	}
}

func TestRemoveStaleAdminSocketNonExistent(t *testing.T) {
	t.Parallel()
	err := removeStaleAdminSocket(filepath.Join(t.TempDir(), "absent.sock"))
	if err != nil {
		t.Fatalf("non-existent socket error = %v", err)
	}
}

func TestRemoveStaleAdminSocketRefusesNonSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := removeStaleAdminSocket(path)
	if err == nil || !strings.Contains(err.Error(), "non-socket") {
		t.Fatalf("regular file error = %v", err)
	}
}

func TestValidateAdminSocketRejectsUnsafePaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"relative.sock",
		"/var/run/socket.sock",
		"/tmp/../etc/evil.sock",
		"/tmp/sub/deep.sock",
		strings.Repeat("/tmp/x", 20),
	} {
		if err := validateAdminSocket(path); err == nil {
			t.Errorf("validateAdminSocket(%q) accepted", path)
		}
	}
}

func TestValidateAdminSocketAcceptsValidPaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		privateAdminSocket(t),
	} {
		if err := validateAdminSocket(path); err != nil {
			t.Errorf("validateAdminSocket(%q) = %v", path, err)
		}
	}
}

// TestRunAccessAllowThinWrapperReportsArgCountError exercises the
// runAccessAllow thin wrapper (which constructs live dependencies) through a
// path that returns before touching K8s or the admin socket: the argument-
// count check in runAccessAllowWithDependencies fires first.

// TestRunUpstreamThinWrapperRoutesToWithDependencies verifies the production
// thin wrapper reaches the argument validation in the WithDependencies
// variant without contacting any external service.
func TestRunUpstreamThinWrapperRoutesToWithDependencies(t *testing.T) {
	t.Parallel()
	err := runUpstream(context.Background(), nil, io.Discard)
	if !errors.Is(err, errUsage) {
		t.Fatalf("runUpstream(nil) error = %v, want errUsage", err)
	}
}

func TestRunRepoThinWrapperRoutesToWithDependencies(t *testing.T) {
	t.Parallel()
	err := runRepo(context.Background(), nil, io.Discard)
	if !errors.Is(err, errUsage) {
		t.Fatalf("runRepo(nil) error = %v, want errUsage", err)
	}
}

func TestRunUplinkThinWrapperRoutesToWithDependencies(t *testing.T) {
	t.Parallel()
	err := runUplink(context.Background(), nil, strings.NewReader(""), io.Discard)
	if !errors.Is(err, errUsage) {
		t.Fatalf("runUplink(nil) error = %v, want errUsage", err)
	}
}

func TestRunInitHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runInit(context.Background(), []string{"--help"}, &output)
	if err != nil {
		t.Fatalf("runInit --help = %v", err)
	}
}

func TestRunInitRejectsPositionalArguments(t *testing.T) {
	t.Parallel()
	err := runInit(context.Background(), []string{"extra"}, io.Discard)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "init accepts flags only") {
		t.Fatalf("runInit extra args error = %v", err)
	}
}

func TestRunInitRejectsRemovedTypeFlag(t *testing.T) {
	t.Parallel()
	err := runInit(context.Background(), []string{"--type", "rust"}, io.Discard)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "flag provided but not defined: -type") {
		t.Fatalf("runInit unknown type error = %v", err)
	}
}

func TestRequestLiveAdminMutationGateFailsWithoutSocket(t *testing.T) {
	t.Parallel()
	// requestLiveAdminMutationGate uses defaultAdminSocket which is
	// /run/oberth/admin.sock. Outside a running daemon, the connection fails.
	err := requestLiveAdminMutationGate(context.Background(), "uplink.register", "/data/oberth.sqlite")
	if err == nil {
		t.Fatal("requestLiveAdminMutationGate succeeded without a daemon socket")
	}
}

func TestRunAccessHelpPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "allow help", args: []string{"allow", "--help"}},
		{name: "revoke help", args: []string{"revoke", "--help"}},
		{name: "list help", args: []string{"list", "--help"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			err := runCLI(context.Background(), append([]string{"access"}, test.args...), nil, &output)
			if err != nil {
				t.Fatalf("runAccess(%q) = %v", test.args, err)
			}
		})
	}
}

func TestAdminGateServerRejectsNilGate(t *testing.T) {
	t.Parallel()
	socket := privateAdminSocket(t)
	server := newAdminGateServer(nil, "/data/oberth.sqlite", nil)
	listener, err := listenAdminSocket(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		_ = removeStaleAdminSocket(socket)
	})
	go func() { _ = server.Serve(listener) }()
	err = requestAdminMutationGate(context.Background(), socket, "uplink.register", "/data/oberth.sqlite")
	if err == nil || !strings.Contains(err.Error(), "mutation gate") {
		t.Fatalf("nil gate error = %v, want mutation gate rejection", err)
	}
}

func TestAdminGateServerRejectsGateError(t *testing.T) {
	t.Parallel()
	socket := privateAdminSocket(t)
	server := newAdminGateServer(func(context.Context) error {
		return errors.New("audit integrity not verified")
	}, "/data/oberth.sqlite", nil)
	listener, err := listenAdminSocket(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		_ = removeStaleAdminSocket(socket)
	})
	go func() { _ = server.Serve(listener) }()
	err = requestAdminMutationGate(context.Background(), socket, "uplink.register", "/data/oberth.sqlite")
	if err == nil || !strings.Contains(err.Error(), "audit integrity not verified") {
		t.Fatalf("gate error = %v, want audit integrity failure", err)
	}
}

// --- JSON output tests (#260) ---

// --- Round-trip tests: real runAccessList output → producer parser → identity equality (#260) ---
// These pin the emitter/parser contract by running the REAL emitter (both
// formats) through the producer's parser and asserting identity-set equality.

// assertIdentitySet checks that identities match a map of
// "upstream/org/repo" → expected grant count.
