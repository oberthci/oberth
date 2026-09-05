package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
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

func TestRunAccessListFormatsGrants(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Grant(context.Background(), "terraform", "*", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Grant(context.Background(), "oberth", "release", "cosign-secret", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runAccessList(context.Background(), []string{"--database", databasePath}, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "REPO") || !strings.Contains(text, "STEP") || !strings.Contains(text, "SECRET") ||
		!strings.Contains(text, "STATUS") {
		t.Fatalf("header missing from listing: %q", text)
	}
	if !strings.Contains(text, "terraform") || !strings.Contains(text, "terraform/credentials") {
		t.Fatalf("first grant missing: %q", text)
	}
	if !strings.Contains(text, "oberth") || !strings.Contains(text, "cosign-secret") {
		t.Fatalf("second grant missing: %q", text)
	}
}

func TestRunAccessListFiltersByRepo(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Grant(context.Background(), "terraform", "*", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Grant(context.Background(), "oberth", "release", "cosign-secret", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runAccessList(context.Background(), []string{"--database", databasePath, "--repo", "terraform"}, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "terraform/credentials") {
		t.Fatalf("filtered grant missing: %q", text)
	}
	if strings.Contains(text, "cosign-secret") {
		t.Fatalf("unfiltered grant present: %q", text)
	}
}

func TestRunAccessListHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if err := runAccessList(context.Background(), []string{"--help"}, &output); err != nil {
		t.Fatal(err)
	}
}

func TestRunAccessListRejectsPositionalArguments(t *testing.T) {
	t.Parallel()
	err := runAccessList(context.Background(), []string{"extra"}, io.Discard)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "no positional arguments") {
		t.Fatalf("positional argument error = %v", err)
	}
}

func TestRunRepoUsageErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"list"},
		{"add"},
		{"add", "--database", filepath.Join(t.TempDir(), "test.sqlite")},
	} {
		err := runRepoWithDependencies(context.Background(), args, io.Discard, repoDependencies{mutationGate: allowTestMutation})
		if err == nil {
			t.Fatalf("runRepo(%q) unexpectedly succeeded", args)
		}
	}
}

func TestRunRepoAddHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runRepoWithDependencies(context.Background(), []string{"add", "--help"}, &output, repoDependencies{mutationGate: allowTestMutation})
	if err != nil {
		t.Fatalf("repo add --help = %v", err)
	}
}

func TestRunRepoAddInvalidDefaultBranch(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, databasePath, "codeberg", "ssh://git@codeberg.org/acme")
	err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "--default-branch", "",
		"widget", "codeberg",
	}, io.Discard, repoDependencies{mutationGate: allowTestMutation})
	if err == nil || !strings.Contains(err.Error(), "default branch is invalid") {
		t.Fatalf("empty default branch error = %v", err)
	}
}

func TestRunUplinkUsageErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"unknown"},
	} {
		err := runUplinkWithDependencies(context.Background(), args, strings.NewReader(""), io.Discard, uplinkDependencies{})
		if err == nil {
			t.Fatalf("runUplink(%q) unexpectedly succeeded", args)
		}
	}
}

func TestRunUplinkListHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runUplinkWithDependencies(context.Background(), []string{"list", "--help"}, strings.NewReader(""), &output, uplinkDependencies{})
	if err != nil {
		t.Fatalf("uplink list --help = %v", err)
	}
}

func TestRunUplinkListRejectsPositionalArgs(t *testing.T) {
	t.Parallel()
	err := runUplinkWithDependencies(context.Background(), []string{"list", "extra"}, strings.NewReader(""), io.Discard, uplinkDependencies{})
	if !errors.Is(err, errUsage) {
		t.Fatalf("uplink list extra arg error = %v", err)
	}
}

func TestRunUplinkRemoveHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runUplinkWithDependencies(context.Background(), []string{"remove", "--help"}, strings.NewReader(""), &output, uplinkDependencies{mutationGate: allowTestMutation})
	if err != nil {
		t.Fatalf("uplink remove --help = %v", err)
	}
}

func TestRunUplinkRemoveRequiresExactlyOneIdentity(t *testing.T) {
	t.Parallel()
	err := runUplinkWithDependencies(context.Background(), []string{"remove"}, strings.NewReader(""), io.Discard, uplinkDependencies{mutationGate: allowTestMutation})
	if !errors.Is(err, errUsage) {
		t.Fatalf("uplink remove without identity error = %v", err)
	}
}

func TestRunUplinkAddHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runUplinkWithDependencies(context.Background(), []string{"add", "--help"}, strings.NewReader(""), &output, uplinkDependencies{mutationGate: allowTestMutation})
	if err != nil {
		t.Fatalf("uplink add --help = %v", err)
	}
}

func TestRunUplinkAddRequiresIdentity(t *testing.T) {
	t.Parallel()
	err := runUplinkWithDependencies(context.Background(), []string{"add"}, strings.NewReader(""), io.Discard, uplinkDependencies{mutationGate: allowTestMutation})
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "public-key source") {
		t.Fatalf("uplink add without args error = %v", err)
	}
}

func TestRunUpstreamUsageErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no subcommand", args: nil, want: "upstream add|list|remove"},
		{name: "unknown subcommand", args: []string{"unknown"}, want: "upstream add"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := runUpstreamWithDependencies(context.Background(), test.args, io.Discard, upstreamDependencies{mutationGate: allowTestMutation})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runUpstream(%q) = %v, want %q", test.args, err, test.want)
			}
		})
	}
}

func TestRunUpstreamAddHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runUpstreamWithDependencies(context.Background(), []string{"add", "--help"}, &output, upstreamDependencies{mutationGate: allowTestMutation})
	if err != nil {
		t.Fatalf("upstream add --help = %v", err)
	}
}

func TestRunUpstreamListHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runUpstreamWithDependencies(context.Background(), []string{"list", "--help"}, &output, upstreamDependencies{mutationGate: allowTestMutation})
	if err != nil {
		t.Fatalf("upstream list --help = %v", err)
	}
}

func TestRunUpstreamRemoveHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runUpstreamWithDependencies(context.Background(), []string{"remove", "--help"}, &output, upstreamDependencies{mutationGate: allowTestMutation})
	if err != nil {
		t.Fatalf("upstream remove --help = %v", err)
	}
}

func TestRunUpstreamRemoveRequiresExactlyOneName(t *testing.T) {
	t.Parallel()
	err := runUpstreamWithDependencies(context.Background(), []string{"remove"}, io.Discard, upstreamDependencies{input: strings.NewReader("yes\n"), mutationGate: allowTestMutation})
	if !errors.Is(err, errUsage) {
		t.Fatalf("upstream remove without name error = %v", err)
	}
}

func TestReadConfirmationVariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input io.Reader
		want  bool
	}{
		{name: "nil reader", input: nil, want: false},
		{name: "empty input", input: strings.NewReader(""), want: false},
		{name: "yes", input: strings.NewReader("yes\n"), want: true},
		{name: "yes with spaces", input: strings.NewReader("  yes  \n"), want: true},
		{name: "no", input: strings.NewReader("no\n"), want: false},
		{name: "YES uppercase", input: strings.NewReader("YES\n"), want: false},
		{name: "y shorthand", input: strings.NewReader("y\n"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := readConfirmation(test.input); got != test.want {
				t.Fatalf("readConfirmation(%q) = %v, want %v", test.name, got, test.want)
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
		"/tmp/oberth-admin.sock",
		"/tmp/test-socket",
	} {
		if err := validateAdminSocket(path); err != nil {
			t.Errorf("validateAdminSocket(%q) = %v", path, err)
		}
	}
}

func TestGatedAdminTokensTouchTokenCredential(t *testing.T) {
	t.Parallel()
	gateCalls := 0
	gateErr := errors.New("gate blocked")
	tokens := gatedAdminTokens{
		Gate: func(_ context.Context, operation string) error {
			gateCalls++
			if operation != "uplink.token.touch" {
				t.Fatalf("unexpected operation %q", operation)
			}
			return gateErr
		},
	}
	err := tokens.TouchTokenCredential(context.Background(), "test-id")
	if !errors.Is(err, gateErr) || gateCalls != 1 {
		t.Fatalf("TouchTokenCredential = %v, calls=%d", err, gateCalls)
	}
}

func TestGatedAdminTokensCreateAndActivateErrorPaths(t *testing.T) {
	t.Parallel()
	gateErr := errors.New("gate blocked")
	tokens := gatedAdminTokens{
		Gate: func(context.Context, string) error { return gateErr },
	}
	if _, err := tokens.CreateTokenCredential(context.Background(), model.TokenCredentialSpec{
		Name: "test", Digest: make([]byte, 32),
	}); !errors.Is(err, gateErr) {
		t.Fatalf("CreateTokenCredential error = %v, want gate error", err)
	}
	if _, err := tokens.ActivateTokenCredential(context.Background(), "test-id"); !errors.Is(err, gateErr) {
		t.Fatalf("ActivateTokenCredential error = %v, want gate error", err)
	}
}

func TestUplinkListShowsAdminLabel(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	digest := make([]byte, 32)
	credential, err := database.CreateTokenCredential(context.Background(), model.TokenCredentialSpec{
		Name: "admin-agent", Digest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RegisterUplink(context.Background(), "admin@test", model.UplinkSpec{
		Fingerprint:       "SHA256:testfingerprintadmin000000000000000000000",
		Identity:          "admin@host",
		TokenCredentialID: credential.ID,
		Admin:             true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runUplinkList(context.Background(), []string{"--database", databasePath}, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "admin@host") || !strings.Contains(text, "yes") {
		t.Fatalf("admin uplink listing = %q", text)
	}
}

func TestDefaultPodNamespaceFallsBackToOberth(t *testing.T) {
	t.Parallel()
	// Outside a pod, /var/run/secrets/kubernetes.io/serviceaccount/namespace
	// does not exist, so it should fall back to "oberth".
	result := defaultPodNamespace()
	if result == "" {
		t.Fatal("defaultPodNamespace returned empty string")
	}
	// In CI this will be "oberth" (fallback); in a real pod it would be the
	// namespace. Either way it must not be empty.
}

// TestRunAccessAllowThinWrapperReportsArgCountError exercises the
// runAccessAllow thin wrapper (which constructs live dependencies) through a
// path that returns before touching K8s or the admin socket: the argument-
// count check in runAccessAllowWithDependencies fires first.
func TestRunAccessAllowThinWrapperReportsArgCountError(t *testing.T) {
	t.Parallel()
	// Wrong number of positional args: runAccessAllow → runAccessAllowWithDependencies
	// → flags.NArg() != 3 → usage error.
	err := runAccessAllow(context.Background(), []string{}, io.Discard)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "access allow") {
		t.Fatalf("runAccessAllow empty args error = %v", err)
	}
}

func TestRunAccessRevokeThinWrapperReportsArgCountError(t *testing.T) {
	t.Parallel()
	err := runAccessRevoke(context.Background(), []string{}, io.Discard)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "access revoke") {
		t.Fatalf("runAccessRevoke empty args error = %v", err)
	}
}

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

func TestRunInitRejectsUnknownType(t *testing.T) {
	t.Parallel()
	err := runInit(context.Background(), []string{"--type", "rust"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unknown project type") {
		t.Fatalf("runInit unknown type error = %v", err)
	}
}

func TestRequestLiveAdminMutationGateFailsWithoutSocket(t *testing.T) {
	t.Parallel()
	// requestLiveAdminMutationGate uses defaultAdminSocket which is
	// /tmp/oberth-admin.sock. Outside a running daemon, the connection fails.
	err := requestLiveAdminMutationGate(context.Background(), "test.operation", "/data/oberth.sqlite")
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
			err := runAccess(context.Background(), test.args, &output)
			if err != nil {
				t.Fatalf("runAccess(%q) = %v", test.args, err)
			}
		})
	}
}

func TestAdminGateServerRejectsNilGate(t *testing.T) {
	t.Parallel()
	socket := filepath.Join("/tmp", "oberth-nilgate-"+filepath.Base(t.TempDir())+".sock")
	server := newAdminGateServer(nil, "/data/oberth.sqlite")
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
	err = requestAdminMutationGate(context.Background(), socket, "test.operation", "/data/oberth.sqlite")
	if err == nil || !strings.Contains(err.Error(), "mutation gate") {
		t.Fatalf("nil gate error = %v, want mutation gate rejection", err)
	}
}

func TestAdminGateServerRejectsGateError(t *testing.T) {
	t.Parallel()
	socket := filepath.Join("/tmp", "oberth-gatefail-"+filepath.Base(t.TempDir())+".sock")
	server := newAdminGateServer(func(context.Context) error {
		return errors.New("audit integrity not verified")
	}, "/data/oberth.sqlite")
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
	err = requestAdminMutationGate(context.Background(), socket, "test.operation", "/data/oberth.sqlite")
	if err == nil || !strings.Contains(err.Error(), "audit integrity not verified") {
		t.Fatalf("gate error = %v, want audit integrity failure", err)
	}
}

func TestRunUpstreamListRejectsPositionalArguments(t *testing.T) {
	t.Parallel()
	err := runUpstreamList(context.Background(), []string{"extra"}, io.Discard)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "no arguments") {
		t.Fatalf("upstream list extra args error = %v", err)
	}
}

func TestRunAccessListShowsRevokedGrants(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Grant(context.Background(), "terraform", "*", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Revoke(context.Background(), "terraform", "*", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Without --revoked, the revoked grant should not appear.
	var output bytes.Buffer
	if err := runAccessList(context.Background(), []string{"--database", databasePath}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "terraform/credentials") {
		t.Fatalf("revoked grant visible without --revoked: %q", output.String())
	}

	// With --revoked, it should appear with a "revoked by" status.
	output.Reset()
	if err := runAccessList(context.Background(), []string{"--database", databasePath, "--revoked"}, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "terraform/credentials") || !strings.Contains(text, "revoked by") {
		t.Fatalf("revoked grant listing = %q", text)
	}
}

func TestRunUpstreamRemoveAutoYes(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, databasePath, "github", "ssh://git@github.com/acme")
	var output bytes.Buffer
	if err := runUpstreamRemove(context.Background(), []string{
		"--database", databasePath, "--yes", "github",
	}, nil, &output, allowTestMutation); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "removed upstream github") {
		t.Fatalf("auto-yes remove output = %q", output.String())
	}
}

// --- JSON output tests (#260) ---

func TestRunAccessListJSONEmitsSchema(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Grant(context.Background(), "github/oberthci/oberth", "release", "oberth/data/release/cosign-secret", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runAccessList(context.Background(), []string{"--database", databasePath, "--json"}, &output); err != nil {
		t.Fatal(err)
	}

	var grants []struct {
		Repo       string  `json:"repo"`
		Step       string  `json:"step"`
		Secret     string  `json:"secret"`
		ApprovedBy string  `json:"approved_by"`
		ApprovedAt string  `json:"approved_at"`
		RevokedBy  string  `json:"revoked_by"`
		RevokedAt  *string `json:"revoked_at"`
	}
	if err := json.Unmarshal(output.Bytes(), &grants); err != nil {
		t.Fatalf("JSON parse failed: %v\nraw output: %s", err, output.String())
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}
	g := grants[0]
	if g.Repo != "github/oberthci/oberth" {
		t.Fatalf("repo = %q", g.Repo)
	}
	if g.Step != "release" {
		t.Fatalf("step = %q", g.Step)
	}
	if g.Secret != "oberth/data/release/cosign-secret" {
		t.Fatalf("secret = %q", g.Secret)
	}
	if g.ApprovedBy != "admin@localhost" {
		t.Fatalf("approved_by = %q", g.ApprovedBy)
	}
	if g.ApprovedAt == "" {
		t.Fatal("approved_at is empty")
	}
	// Active grant — revocation fields should be absent/zero.
	if g.RevokedBy != "" {
		t.Fatalf("revoked_by should be empty for active grant, got %q", g.RevokedBy)
	}
	if g.RevokedAt != nil {
		t.Fatalf("revoked_at should be nil for active grant, got %v", g.RevokedAt)
	}
}

func TestRunAccessListJSONZeroGrantsEmitsEmptyArray(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runAccessList(context.Background(), []string{"--database", databasePath, "--json"}, &output); err != nil {
		t.Fatal(err)
	}

	text := strings.TrimSpace(output.String())
	if text != "[]" {
		t.Fatalf("zero grants should emit '[]', got: %q", text)
	}
}

func TestRunAccessListJSONRevokedGrant(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Grant(context.Background(), "terraform", "*", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Revoke(context.Background(), "terraform", "*", "terraform/credentials", "revoker@localhost"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runAccessList(context.Background(), []string{"--database", databasePath, "--json", "--revoked"}, &output); err != nil {
		t.Fatal(err)
	}

	var grants []struct {
		RevokedBy string  `json:"revoked_by"`
		RevokedAt *string `json:"revoked_at"`
	}
	if err := json.Unmarshal(output.Bytes(), &grants); err != nil {
		t.Fatalf("JSON parse failed: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 revoked grant, got %d", len(grants))
	}
	if grants[0].RevokedBy != "revoker@localhost" {
		t.Fatalf("revoked_by = %q", grants[0].RevokedBy)
	}
	if grants[0].RevokedAt == nil {
		t.Fatal("revoked_at should be present for revoked grant")
	}
}

func TestRunAccessListJSONDefaultOutputUnchanged(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Grant(context.Background(), "terraform", "*", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Default (no --json) should still produce tabwriter output.
	var output bytes.Buffer
	if err := runAccessList(context.Background(), []string{"--database", databasePath}, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "REPO") || !strings.Contains(text, "STATUS") {
		t.Fatalf("tabwriter header missing from default output: %q", text)
	}
	// Verify it is NOT JSON.
	if strings.HasPrefix(strings.TrimSpace(text), "[") {
		t.Fatalf("default output should not be JSON: %q", text)
	}
}

// --- Round-trip tests: real runAccessList output → producer parser → identity equality (#260) ---
// These pin the emitter/parser contract by running the REAL emitter (both
// formats) through the producer's parser and asserting identity-set equality.

func TestRoundTripTabwriterOutputThroughProducerParser(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Seed qualified grants.
	for _, g := range []struct{ repo, step, secret string }{
		{"codeberg/oberthci/oberth", "release", "oberth/data/release/cosign-secret"},
		{"codeberg/oberthci/oberth", "release", "oberth/data/release/r2-upload-token"},
		{"github/skipops/terraform", "*", "oberth/upstream/skipops/terraform/gcp-sa"},
	} {
		if _, err := database.Grant(context.Background(), g.repo, g.step, g.secret, "admin@localhost"); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Produce the real tabwriter output.
	var tabOutput bytes.Buffer
	if err := runAccessList(context.Background(), []string{"--database", databasePath}, &tabOutput); err != nil {
		t.Fatal(err)
	}

	// Feed it through the installer's tabwriter parser.
	identities, err := installer.ParseAccessListOutput(tabOutput.Bytes())
	if err != nil {
		t.Fatalf("tabwriter round-trip parse failed: %v\nraw output:\n%s", err, tabOutput.String())
	}

	assertIdentitySet(t, identities, map[string]int{
		"codeberg/oberthci/oberth": 2,
		"github/skipops/terraform": 1,
	})
}

func TestRoundTripJSONOutputThroughProducerParser(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct{ repo, step, secret string }{
		{"codeberg/oberthci/oberth", "release", "oberth/data/release/cosign-secret"},
		{"codeberg/oberthci/oberth", "release", "oberth/data/release/r2-upload-token"},
		{"github/skipops/terraform", "*", "oberth/upstream/skipops/terraform/gcp-sa"},
	} {
		if _, err := database.Grant(context.Background(), g.repo, g.step, g.secret, "admin@localhost"); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Produce the real JSON output.
	var jsonOutput bytes.Buffer
	if err := runAccessList(context.Background(), []string{"--database", databasePath, "--json"}, &jsonOutput); err != nil {
		t.Fatal(err)
	}

	// Feed it through the installer's JSON parser.
	identities, err := installer.ParseAccessListJSON(jsonOutput.Bytes())
	if err != nil {
		t.Fatalf("JSON round-trip parse failed: %v\nraw output:\n%s", err, jsonOutput.String())
	}

	assertIdentitySet(t, identities, map[string]int{
		"codeberg/oberthci/oberth": 2,
		"github/skipops/terraform": 1,
	})
}

// assertIdentitySet checks that identities match a map of
// "upstream/org/repo" → expected grant count.
func assertIdentitySet(t *testing.T, identities []installer.PerRepoIdentity, want map[string]int) {
	t.Helper()
	if len(identities) != len(want) {
		t.Fatalf("expected %d identities, got %d: %+v", len(want), len(identities), identities)
	}
	for _, id := range identities {
		key := id.Upstream + "/" + id.Org + "/" + id.Repo
		expectedGrants, ok := want[key]
		if !ok {
			t.Fatalf("unexpected identity %q", key)
		}
		if len(id.Grants) != expectedGrants {
			t.Fatalf("identity %q: expected %d grants, got %d: %v", key, expectedGrants, len(id.Grants), id.Grants)
		}
	}
}
