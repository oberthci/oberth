package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Tests for the non-interactive install a script or an AI agent runs: no
// secret on the output, client files written, onboarding from flags
// (findings #4, #6, #10 of the fresh-install rehearsal).

const testOnceCredential = "s.not-a-real-root-token"

func TestSecretsFilePathRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "target"), link); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(worktree, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A linked worktree or submodule has a .git file, not a directory.
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		existing: "already exists",
		link:     "already exists",
		filepath.Join(dir, "missing", "secrets.txt"):  "directory",
		filepath.Join(worktree, "sub", "secrets.txt"): "git worktree",
		"": "needs a path",
	} {
		if _, err := CheckSecretsFilePath(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", path, err, want)
		}
	}
	if _, err := CheckSecretsFilePath(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatalf("a new file outside a worktree: %v", err)
	}
}

func TestSecretsFileHoldsCredentialsAndOutputOnlyItsDigest(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	path := filepath.Join(t.TempDir(), "secrets.txt")
	secrets, err := createSecretsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets.add("Root token", testOnceCredential)
	secrets.add("Unseal key", testOnceCredential+"-unseal")
	secrets.add("Bearer token", testOnceCredential+"-bearer")
	var out bytes.Buffer
	secrets.finish(&out)

	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"OPENBAO_ROOT_TOKEN=" + testOnceCredential + "\n", "OPENBAO_UNSEAL_KEY=" + testOnceCredential + "-unseal\n", SecretsFileTokenKey + "=" + testOnceCredential + "-bearer\n"} {
		if !bytes.Contains(raw, []byte(line)) {
			t.Errorf("the secrets file lacks a line for %s", strings.SplitN(line, "=", 2)[0])
		}
	}
	if strings.Contains(out.String(), testOnceCredential) {
		t.Fatal("a credential reached the output")
	}
	if !strings.Contains(out.String(), path) || !strings.Contains(out.String(), "sha256 ") || !strings.Contains(out.String(), "3 values") {
		t.Fatalf("output does not name the file and its digest:\n%s", out.String())
	}
}

func TestSecretsFileWithoutCredentialsIsRemoved(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "secrets.txt")
	secrets, err := createSecretsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	secrets.finish(&out)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an empty secrets file was kept: %v", err)
	}
}

func noClusterInstallDeps(t *testing.T, outputIsTerminal bool) InstallDeps {
	return InstallDeps{
		Output:           io.Discard,
		GOOS:             "linux",
		LookPath:         func(name string) (string, error) { return "/usr/bin/" + name, nil },
		IsTerminal:       func() bool { return false },
		OutputIsTerminal: func() bool { return outputIsTerminal },
		LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
			return nil, nil, "", errors.New("stop: cluster reached")
		},
	}
}

// Without a terminal and without --secrets-file, an install that can create
// credentials refuses before it touches the cluster.
func TestInstallRefusesToPrintCredentialsWithoutATerminal(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{
		{InstallSecretStore: true},
		{InstallSecretStoreDev: true, ForgeURL: "ssh://git@forge.example.test/acme"},
	} {
		err := Execute(context.Background(), cfg, noClusterInstallDeps(t, false))
		if err == nil || !strings.Contains(err.Error(), "--secrets-file") {
			t.Fatalf("%+v: got %v", cfg, err)
		}
	}
	// On a terminal, or when nothing can be created, the install goes on.
	for _, test := range []struct {
		cfg      Config
		terminal bool
	}{{Config{InstallSecretStore: true}, true}, {Config{InstallSecretStoreDev: true}, false}} {
		err := Execute(context.Background(), test.cfg, noClusterInstallDeps(t, test.terminal))
		if err == nil || !strings.Contains(err.Error(), "stop: cluster reached") {
			t.Fatalf("%+v: got %v", test.cfg, err)
		}
	}
}

// --secrets-file is checked and reserved before the cluster is contacted, and
// a run that created nothing leaves no file.
func TestInstallReservesTheSecretsFileFirst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.txt")
	err := Execute(context.Background(), Config{InstallSecretStore: true, SecretsFile: path}, noClusterInstallDeps(t, false))
	if err == nil || !strings.Contains(err.Error(), "stop: cluster reached") {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a secrets file without credentials was kept: %v", statErr)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = Execute(context.Background(), Config{InstallSecretStore: true, SecretsFile: path}, noClusterInstallDeps(t, false))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("an existing secrets file: %v", err)
	}
}

// A non-interactive install without an upstream still configures this
// machine: client files with the client-visible ports.
func TestNonInteractiveInstallWithoutUpstreamWritesClientFiles(t *testing.T) {
	t.Parallel()
	host := &fakeOberthHost{t: t}
	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, strings.NewReader(""), false)
	cfg := Config{ClientHost: "localhost", ClientSSHPort: "31022", ClientHTTPSPort: "31443"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	err := finishInstallTest(context.Background(), cfg, deps, &buf)
	if !errors.Is(err, ErrOnboardingPartial) {
		t.Fatalf("got %v", err)
	}
	root, _ := deps.ClientConfigRoot()
	env, err := os.ReadFile(filepath.Join(root, "env"))
	if err != nil {
		t.Fatalf("no env file: %v\n%s", err, buf.String())
	}
	if !strings.Contains(string(env), `OBERTH_BASE_URL="https://localhost:31443"`) {
		t.Fatalf("env:\n%s", env)
	}
	output := buf.String()
	for _, want := range []string{"ssh://git@localhost:31022/", "--upstream-url", "--expected-host-fingerprint"} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, ":30022") || strings.Contains(output, ":30443") {
		t.Errorf("output names a default port:\n%s", output)
	}
}

// Onboarding from flags alone: the identity defaults to user@host, a key pair
// is generated when ~/.ssh has none, and the expected host fingerprint
// reaches the pod's upstream add.
func TestNonInteractiveOnboardingFromFlags(t *testing.T) {
	t.Parallel()
	host := &fakeOberthHost{t: t, uplinkOutput: "Uplink token for tester@box (shown once):\n" + testOnceCredential + "\n"}
	var buf bytes.Buffer
	deps := onboardingDeps(t, host, &buf, strings.NewReader(""), false)
	home, _ := deps.UserHomeDir()
	var delivered []string
	cfg := Config{ForgeURL: "ssh://git@forge.example.test/acme", ExpectedHostFingerprint: "SHA256:" + strings.Repeat("A", 43), ClientAccess: "none"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	tw := newTableWriter(&buf, false)
	tw.WriteHeader()
	tw.WriteFooter()
	var creds heldCredentials
	deps.CredentialSink = func(label, value string) { delivered = append(delivered, label) }
	if err := FinishInstall(context.Background(), cfg, deps, tw, &creds); err != nil && !errors.Is(err, ErrOnboardingPartial) {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if len(host.addArgv) == 0 || !slices.Contains(host.addArgv[0], "--expected-host-fingerprint") || !slices.Contains(host.addArgv[0], cfg.ExpectedHostFingerprint) {
		t.Fatalf("upstream add argv %q", host.addArgv)
	}
	if !strings.Contains(strings.Join(host.uplinkArgv, " "), "uplink add - ") {
		t.Fatalf("uplink argv %q", host.uplinkArgv)
	}
	generated := filepath.Join(home, ".ssh", generatedUplinkKeyName)
	if info, err := os.Stat(generated); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("generated private key: %v %v", info, err)
	}
	if !strings.HasPrefix(host.uplinkStdin, "ssh-ed25519 ") {
		t.Fatalf("the uplink got %q, not the generated public key", host.uplinkStdin)
	}
	if !slices.Contains(delivered, "Bearer token") || strings.Contains(buf.String(), testOnceCredential) {
		t.Fatalf("token delivery %v; output:\n%s", delivered, buf.String())
	}
}

func TestExpectedHostFingerprintValidation(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{
		{ExpectedHostFingerprint: "SHA256:" + strings.Repeat("A", 43)},
		{ForgeURL: "ssh://git@forge.example.test/acme", ExpectedHostFingerprint: "MD5:aa"},
		{ForgeURL: "ssh://git@forge.example.test/acme", ClientSSHPort: "70000"},
		{ForgeURL: "ssh://git@forge.example.test/acme", ClientHTTPSPort: "0443"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%+v validated", cfg)
		}
	}
	cfg := Config{ForgeURL: "ssh://git@forge.example.test/acme", ExpectedHostFingerprint: "SHA256:" + strings.Repeat("A", 43)}
	if got := cfg.expectedFingerprintFor("ssh://git@other.example.test/acme"); got != "" {
		t.Fatalf("another host got the fingerprint: %q", got)
	}
}

// Any cluster is a valid target, but the install never picks one of several
// contexts itself: --context names it.
func TestInstallNeedsContextWhenKubeconfigHasSeveral(t *testing.T) {
	t.Parallel()
	deps := noClusterInstallDeps(t, true)
	deps.KubeContexts = func(string) ([]string, error) { return []string{"lab", "prod"}, nil }
	err := Execute(context.Background(), Config{InstallSecretStore: true}, deps)
	if err == nil || !strings.Contains(err.Error(), "--context") || !strings.Contains(err.Error(), "prod") {
		t.Fatalf("got %v", err)
	}
	err = Execute(context.Background(), Config{InstallSecretStore: true, ContextName: "prod"}, deps)
	if err == nil || !strings.Contains(err.Error(), "stop: cluster reached") {
		t.Fatalf("with --context: %v", err)
	}
	deps.KubeContexts = func(string) ([]string, error) { return []string{"only"}, nil }
	err = Execute(context.Background(), Config{InstallSecretStore: true}, deps)
	if err == nil || !strings.Contains(err.Error(), "stop: cluster reached") {
		t.Fatalf("one context: %v", err)
	}
}
