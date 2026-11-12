package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testToken = "oberth_secrettokenvalue000000000"

func clientAccessDeps(t *testing.T, input string) (Deps, *bytes.Buffer, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	var out bytes.Buffer
	deps := Deps{
		Output:     &out,
		Input:      strings.NewReader(input),
		IsTerminal: func() bool { return true },
		// Deny every host binary by default. Without this the tests find the
		// real `claude` on the developer's PATH and register a server in their
		// own configuration, which is a test suite editing the machine it runs
		// on. A test that wants that path stubs LookPath itself.
		LookPath: func(string) (string, error) { return "", errors.New("not found in test") },
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			if name == "kubectl" && len(args) > 0 && strings.Contains(strings.Join(args, " "), "/usr/local/bin/oberth admin read-file /run/oberth-identities/server/ca.crt") {
				return []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"), nil
			}
			return nil, fmt.Errorf("test ran an unstubbed command: %s %s", name, strings.Join(args, " "))
		},
		KubeClient: fake.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "oberth-tls", Namespace: "oberth"},
			Data:       map[string][]byte{"tls.crt": []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n")},
		}),
	}
	return deps, &out, home
}

// The one moment the bearer token exists in this process is right after the
// uplink is registered. Everything the CLI and an MCP client need is derivable
// there and nowhere else without asking the operator to paste it back.
func TestClientAccessWritesWhatEachClientNeeds(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		answer  string
		wantEnv bool
		wantMCP bool
		wantCA  bool
	}{
		{"both is the default", "\n", true, true, true},
		{"cli only", "c\n", true, false, true},
		{"mcp only", "m\n", false, true, true},
		{"neither", "n\n", false, false, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			deps, _, home := clientAccessDeps(t, testCase.answer)
			tw := newTableWriter(deps.Output, false)

			if err := offerClientAccess(context.Background(), Config{}, deps, tw, testToken); err != nil {
				t.Fatalf("offer returned an error, which must never fail an install: %v", err)
			}

			envPath := filepath.Join(home, ".config", "oberth", "env")
			mcpPath := filepath.Join(home, ".config", "oberth", "mcp.json")
			caPath := filepath.Join(home, ".config", "oberth", "ca.crt")

			assertExists(t, envPath, testCase.wantEnv)
			assertExists(t, mcpPath, testCase.wantMCP)
			assertExists(t, caPath, testCase.wantCA)
		})
	}
}

// The contract docs/remote-cli.md states is that no token is written to disk.
// This is the test that has to be able to fail: it reads every file the offer
// produced and looks for the token itself.
func TestClientAccessNeverWritesTheTokenToDisk(t *testing.T) {
	deps, out, home := clientAccessDeps(t, "\n")
	tw := newTableWriter(deps.Output, false)

	if err := offerClientAccess(context.Background(), Config{}, deps, tw, testToken); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(home, ".config", "oberth")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("nothing was written: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the default choice wrote no files")
	}
	for _, entry := range entries {
		body, readErr := os.ReadFile(filepath.Join(root, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(body), testToken) {
			t.Errorf("%s contains the bearer token; it must come from the token command instead", entry.Name())
		}
	}
	if strings.Contains(out.String(), testToken) {
		t.Error("the offer echoed the token into its own output")
	}
}

// Nothing here may fail an install. A cluster that will not answer for its own
// TLS Secret is a reason to tell the operator to finish by hand, not a reason
// to abandon a deployment that is otherwise up.
func TestClientAccessSurvivesAClusterThatWillNotAnswer(t *testing.T) {
	deps, _, _ := clientAccessDeps(t, "\n")
	deps.KubeClient = fake.NewSimpleClientset() // no oberth-tls Secret
	tw := newTableWriter(deps.Output, false)

	if err := offerClientAccess(context.Background(), Config{}, deps, tw, testToken); err != nil {
		t.Fatalf("a missing TLS Secret failed the install: %v", err)
	}
}

// Without a terminal nobody answers the offer, so it takes the default Enter
// would (both clients) without reading the closed stdin, and the CA is written
// owner-only whatever the umask (#6, #10 of the fresh-install rehearsal).
func TestClientAccessDefaultsToBothWithoutATerminal(t *testing.T) {
	deps, out, home := clientAccessDeps(t, "")
	deps.IsTerminal = func() bool { return false }
	tw := newTableWriter(deps.Output, false)
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)

	if err := offerClientAccess(context.Background(), Config{}, deps, tw, testToken); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config", "oberth")
	assertExists(t, filepath.Join(root, "env"), true)
	assertExists(t, filepath.Join(root, "mcp.json"), true)
	info, err := os.Stat(filepath.Join(root, "ca.crt"))
	if err != nil || info.Mode().Perm()&0o022 != 0 {
		t.Fatalf("ca.crt mode: %v %v", info, err)
	}
	if strings.Contains(out.String(), testToken) {
		t.Fatal("the token reached the output")
	}

	// A rerun that plans the same files finds nothing to refuse.
	if err := offerClientAccess(context.Background(), Config{}, deps, newTableWriter(deps.Output, false), testToken); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "--replace-client-files") {
		t.Fatalf("an identical rerun was refused:\n%s", out.String())
	}

	// Files that say something else are the operator's: without a terminal
	// nothing is written, all three stay, and the output names the flag.
	env := filepath.Join(root, "env")
	const other = "export OBERTH_BASE_URL=\"https://other.example.test:30443\"\n"
	if err := os.WriteFile(env, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	mcpBefore, err := os.ReadFile(filepath.Join(root, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mcp.json"), append(mcpBefore, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := offerClientAccess(context.Background(), Config{}, deps, newTableWriter(deps.Output, false), testToken); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(env); string(got) != other {
		t.Fatalf("another server's env was replaced:\n%s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "mcp.json")); !bytes.Equal(got, append(mcpBefore, '\n')) {
		t.Fatal("mcp.json was replaced although env was refused")
	}
	for _, want := range []string{"--replace-client-files", "--client-access none", "env", "mcp.json", "kept"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the refusal lacks %q:\n%s", want, out.String())
		}
	}
	// --replace-client-files replaces them.
	if err := offerClientAccess(context.Background(), Config{ReplaceClientFiles: true}, deps, newTableWriter(deps.Output, false), testToken); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(env); string(got) == other || !strings.Contains(string(got), "OBERTH_TOKEN_COMMAND") {
		t.Fatalf("--replace-client-files did not replace env:\n%s", got)
	}

	// --client-access none still declines in advance.
	deps, _, home = clientAccessDeps(t, "")
	deps.IsTerminal = func() bool { return false }
	if err := offerClientAccess(context.Background(), Config{ClientAccess: "none"}, deps, newTableWriter(deps.Output, false), testToken); err != nil {
		t.Fatal(err)
	}
	assertExists(t, filepath.Join(home, ".config", "oberth", "env"), false)
}

// With --secrets-file the token is never on screen: the notes name its line
// and a command that moves it into the secret store unseen.
func TestClientAccessNotesReadTheTokenFromTheSecretsFile(t *testing.T) {
	var out bytes.Buffer
	printClientAccessNotes(&out, "/srv/u/.config/oberth", clientAccessBoth, "secret-tool store --label=x service oberth", true, "/srv/u/oberth secrets.txt", false)
	want := "sed -n 's/^OBERTH_UPLINK_TOKEN=//p' '/srv/u/oberth secrets.txt' | secret-tool store --label=x service oberth"
	if !strings.Contains(out.String(), want) || strings.Contains(out.String(), "token above") {
		t.Fatalf("notes:\n%s", out.String())
	}
	// macOS: security -i reads the command from stdin, so the token is in no
	// argument list (security's -w takes a value only as an argument).
	if got, want := storeFromSecretsFile("security add-generic-password -s oberth-token -a \"$USER\" -w", "/s", false),
		`sed -n "s|^OBERTH_UPLINK_TOKEN=\(.*\)|add-generic-password -U -s oberth-token -a $USER -w \1|p" /s | security -i`; got != want {
		t.Fatalf("macOS store command:\n got %s\nwant %s", got, want)
	}
	// pass insert asks twice on a pipe; -e reads the one line.
	if got := storeFromSecretsFile("pass insert oberth/token", "/s", false); got != "sed -n 's/^OBERTH_UPLINK_TOKEN=//p' /s | pass insert -e -f oberth/token" {
		t.Fatalf("pass store command: %s", got)
	}
}

func assertExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	switch {
	case want && err != nil:
		t.Errorf("%s was not written", filepath.Base(path))
	case !want && err == nil:
		t.Errorf("%s was written but not asked for", filepath.Base(path))
	}
}

// Writing ~/.config/oberth/mcp.json configures nothing: no client reads that
// path. When Claude Code is on PATH the install sets up its plugin through
// Claude Code's own commands, registers no MCP server besides it, and passes
// the token in no argument list.
func TestClientAccessInstallsThePluginWhenClaudeCodeIsPresent(t *testing.T) {
	deps, _, _ := clientAccessDeps(t, "\n")
	claude := &fakeClaude{t: t}
	deps.LookPath = claude.deps().LookPath
	deps.RunCommand = func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		if name == "kubectl" && len(args) > 0 && strings.Contains(strings.Join(args, " "), "/usr/local/bin/oberth admin read-file /run/oberth-identities/server/ca.crt") {
			return []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"), nil
		}
		return claude.run(ctx, input, name, args...)
	}
	tw := newTableWriter(deps.Output, false)

	if err := offerClientAccess(context.Background(), Config{}, deps, tw, testToken); err != nil {
		t.Fatal(err)
	}
	if !claude.installed {
		t.Fatalf("claude is on PATH but the plugin was never installed; commands run: %v", claude.commands())
	}
	for _, command := range claude.commands() {
		if strings.Contains(command, " mcp ") {
			t.Errorf("an MCP server was registered besides the plugin: %s", command)
		}
		if strings.Contains(command, testToken) {
			t.Error("a command carries the token in its arguments")
		}
	}
}

// Without the client installed there is nothing to register, and the written
// file plus the printed instruction are the whole answer.
func TestClientAccessDoesNotInventAClientThatIsNotInstalled(t *testing.T) {
	deps, _, _ := clientAccessDeps(t, "\n")
	deps.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	var ran int
	deps.RunCommand = func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		if name == "kubectl" {
			return []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"), nil
		}
		ran++
		return nil, nil
	}
	tw := newTableWriter(deps.Output, false)

	if err := offerClientAccess(context.Background(), Config{}, deps, tw, testToken); err != nil {
		t.Fatal(err)
	}
	if ran != 0 {
		t.Errorf("ran %d commands with no client installed", ran)
	}
}

// The offer previously lived only inside first-time onboarding, so once an
// upstream existed the install went straight to "Ready" and there was no way to
// configure a client afterwards. Anyone who declined it, whose install failed
// partway, or who added a second machine had to reconstruct three environment
// variables and a CA by hand from documentation.
func TestClientAccessIsOfferedOnAReadyDeploymentThatHasNoConfigYet(t *testing.T) {
	deps, _, home := clientAccessDeps(t, "\n")
	tw := newTableWriter(deps.Output, false)

	if err := offerClientAccessToConfiguredDeployment(context.Background(), Config{}, deps, tw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "oberth", "env")); err != nil {
		t.Error("a ready deployment with no client configuration was not offered one")
	}
}

// It must stay quiet when the machine is already configured, or every install
// on a working setup ends with a prompt that has nothing to do.
func TestClientAccessIsNotReofferedWhenItIsAlreadyConfigured(t *testing.T) {
	deps, _, home := clientAccessDeps(t, "\n")
	root := filepath.Join(home, ".config", "oberth")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "env"), []byte("export OBERTH_BASE_URL=x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tw := newTableWriter(deps.Output, false)

	if err := offerClientAccessToConfiguredDeployment(context.Background(), Config{}, deps, tw); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "export OBERTH_BASE_URL=x\n" {
		t.Error("an existing client configuration was overwritten without being asked about")
	}
}

// A machine whose installer-written client config pins an old certificate
// gets the server's current CA, without a terminal or a prompt.
func TestRefreshPinnedServerCARewritesStaleCA(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	root := filepath.Join(home, ".config", "oberth")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"env": "export OBERTH_BASE_URL=x\n", "ca.crt": "old\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newCA := string(newTestServerCA(t, []string{"oberth"}))
	var table bytes.Buffer
	deps := Deps{
		Output: &table,
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			return []byte(newCA), nil
		},
	}
	refreshPinnedServerCA(context.Background(), Config{}, deps, newTableWriter(&table, false))
	got, err := os.ReadFile(filepath.Join(root, "ca.crt"))
	if err != nil || string(got) != newCA {
		t.Fatalf("ca.crt = %q, %v", got, err)
	}
	if !strings.Contains(table.String(), "refreshed") {
		t.Fatalf("refresh not reported: %q", table.String())
	}
	info, _ := os.Stat(filepath.Join(root, "ca.crt"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ca.crt mode %v", info.Mode().Perm())
	}
}

// A choice made in advance (the setup wizard's client-access screen, or
// --client-access) is honoured without a terminal: the wizard runs the
// installer non-interactively and must still get the files it asked for.
func TestClientAccessDecidedInAdvanceNeedsNoTerminal(t *testing.T) {
	deps, _, home := clientAccessDeps(t, "")
	deps.IsTerminal = func() bool { return false }
	deps.Input = nil
	tw := newTableWriter(deps.Output, false)

	if err := offerClientAccess(context.Background(), Config{ClientAccess: "cli"}, deps, tw, testToken); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config", "oberth")
	assertExists(t, filepath.Join(root, "env"), true)
	assertExists(t, filepath.Join(root, "mcp.json"), false)

	if err := (&Config{ClientAccess: "everything"}).Validate(); err == nil || !strings.Contains(err.Error(), "--client-access") {
		t.Fatalf("an unknown client-access choice must be rejected, got %v", err)
	}
}
