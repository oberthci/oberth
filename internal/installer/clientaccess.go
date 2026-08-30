package installer

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Client access is offered here, immediately after the uplink is registered,
// because this is the only moment the bearer token exists in this process.
// Every other placement has to ask the operator to paste back a credential the
// install just printed, and a credential that gets pasted is a credential that
// ends up in a shell history.
//
// Nothing in this file may fail an install. A cluster that will not answer for
// its own TLS Secret, a home directory that will not accept a write, a refused
// prompt: each of them means the operator finishes by hand, which is what the
// table row says. Only ErrInterrupted propagates.

const (
	clientAccessBoth = iota
	clientAccessCLI
	clientAccessMCP
	clientAccessNeither
)

// httpsNodePort is the chart's fixed HTTPS NodePort, the address the dashboard
// and both clients reach the server on.
const httpsNodePort = "30443"

func offerClientAccess(ctx context.Context, cfg Config, deps Deps, tw *tableWriter, token string) error {
	// Deliberately narrower than isInteractive: this offer prompts and writes
	// files, but never drives an interactive subprocess, so RunInteractive is
	// not among what it needs. Asking for more than it uses would make it
	// untestable on its own and would skip silently in a session that could in
	// fact answer.
	if deps.IsTerminal == nil || !deps.IsTerminal() || deps.Input == nil {
		return nil
	}
	if strings.TrimSpace(token) == "" {
		// No token means no uplink was registered, so there is nothing either
		// client could authenticate with.
		tw.AppendRow("Client access", "no uplink token", "⚠ manual", false)
		return nil
	}
	return runClientAccessOffer(ctx, cfg, deps, tw, true)
}

// offerClientAccessToConfiguredDeployment reaches the same offer on an install
// that had nothing to onboard.
//
// The offer previously lived only inside first-time onboarding, so a deployment
// that already had an upstream went straight to "Ready" and never mentioned it.
// Anyone who declined it once, whose install failed partway, or who is setting
// up a second machine had no route back: they reconstructed three environment
// variables and a CA by hand from documentation.
//
// Nothing is written when this machine is already configured, or a working
// setup would end every install with a prompt that has nothing to do. No fresh
// token exists on this path and none is needed: the files name a command that
// reads one, they never hold it.
func offerClientAccessToConfiguredDeployment(ctx context.Context, cfg Config, deps Deps, tw *tableWriter) error {
	if deps.IsTerminal == nil || !deps.IsTerminal() || deps.Input == nil {
		return nil
	}
	root, err := clientConfigRoot()
	if err != nil {
		return nil
	}
	if _, statErr := os.Stat(filepath.Join(root, "env")); statErr == nil {
		return nil
	}
	if _, statErr := os.Stat(filepath.Join(root, "mcp.json")); statErr == nil {
		return nil
	}
	return runClientAccessOffer(ctx, cfg, deps, tw, false)
}

func runClientAccessOffer(ctx context.Context, cfg Config, deps Deps, tw *tableWriter, freshToken bool) error {
	w := deps.Output
	color := isColor(deps)

	choice, err := selectOption(ctx, deps, color, "Client access",
		[]string{"Both", "CLI only", "MCP only", "Neither"}, clientAccessBoth)
	if err != nil {
		if errors.Is(err, ErrInterrupted) {
			return ErrInterrupted
		}
		tw.AppendRow("Client access", "not configured", "⚠ manual", false)
		return nil
	}
	if choice < 0 {
		choice = clientAccessBoth
	}
	if choice == clientAccessNeither {
		tw.AppendRow("Client access", "declined", "skipped", false)
		return nil
	}

	root, err := clientConfigRoot()
	if err != nil {
		tw.AppendRow("Client access", "no config directory", "⚠ manual", false)
		return nil
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		tw.AppendRow("Client access", displayPath(root), "✗ error", false)
		return nil
	}

	// The certificate is the deployment's, so it is read from the deployment
	// rather than reconstructed. Since Oberth #236 OBERTH_CA_CERT replaces the
	// system pool instead of adding to it, so this file is required even where
	// the signer would otherwise be trusted.
	caPath := filepath.Join(root, "ca.crt")
	authority, err := serverCACertificate(ctx, cfg, deps)
	if err != nil {
		tw.AppendRow("Client access", "TLS certificate unreadable", "⚠ manual", false)
		return nil
	}
	if err := atomicWriteFile(caPath, authority, 0600); err != nil {
		tw.AppendRow("Client access", displayPath(caPath), "✗ error", false)
		return nil
	}

	baseURL := "https://" + clientReachableHost(deps) + ":" + httpsNodePort
	tokenCommand, tokenHint := tokenCommandForHost()

	if choice == clientAccessBoth || choice == clientAccessCLI {
		path := filepath.Join(root, "env")
		if err := atomicWriteFile(path, []byte(renderClientEnv(baseURL, caPath, tokenCommand)), 0600); err != nil {
			tw.AppendRow("CLI access", displayPath(path), "✗ error", false)
		} else {
			tw.AppendRow("CLI access", displayPath(path), "✓ written", false)
			// The file exists now, so the only remaining step is the one the
			// install used to leave as a printed instruction.
			wireShellProfile(ctx, cfg, deps, tw, path)
		}
	}
	if choice == clientAccessBoth || choice == clientAccessMCP {
		path := filepath.Join(root, "mcp.json")
		body, marshalErr := renderMCPConfig(baseURL, tokenCommand)
		switch {
		case marshalErr != nil:
			tw.AppendRow("MCP access", displayPath(path), "✗ error", false)
		case atomicWriteFile(path, body, 0600) != nil:
			tw.AppendRow("MCP access", displayPath(path), "✗ error", false)
		default:
			// The file alone configures nothing: no client reads this path. It
			// is written as the record, and the client is registered through
			// its own documented command where one exists.
			if registerWithClaudeCode(ctx, deps, body) {
				tw.AppendRow("MCP access", "registered with Claude Code", "✓ ready", false)
			} else {
				tw.AppendRow("MCP access", displayPath(path), "✓ written", false)
			}
		}
	}

	printClientAccessNotes(w, root, choice, tokenHint, freshToken)
	return nil
}

// renderClientEnv writes what the CLI reads and deliberately not the token.
// OBERTH_TOKEN_COMMAND names a command whose output is the token, so the
// credential lives in the platform's secret store and this file can be read by
// anything without leaking one.
func renderClientEnv(baseURL, caPath, tokenCommand string) string {
	return fmt.Sprintf(`# Written by oberth install. Source it, or add it to your shell profile:
#
#     . %s
#
# No token is stored here. OBERTH_TOKEN_COMMAND names a command whose standard
# output is the bearer token, so it comes from your secret store and never
# lands in a file. Replace it if you keep credentials somewhere else.
export OBERTH_BASE_URL=%q
export OBERTH_CA_CERT=%q
export OBERTH_TOKEN_COMMAND=%q
`, displayPath(filepath.Join(filepath.Dir(caPath), "env")), baseURL, caPath, tokenCommand)
}

// renderMCPConfig uses headersHelper rather than a literal Authorization
// header, so the same token command serves both clients and neither config
// file holds a credential. The helper is Claude Code's; a client that does not
// implement it needs a literal header, which is why the install prints that
// rather than writing a credential for a client we cannot check.
func renderMCPConfig(baseURL, tokenCommand string) ([]byte, error) {
	type server struct {
		Type          string `json:"type"`
		URL           string `json:"url"`
		HeadersHelper string `json:"headersHelper"`
	}
	document := map[string]map[string]server{
		"mcpServers": {
			"oberth": {
				Type: "http",
				URL:  baseURL + "/mcp",
				HeadersHelper: fmt.Sprintf(
					`printf '{"Authorization":"Bearer %%s"}' "$(%s)"`, tokenCommand),
			},
		},
	}
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// serverCACertificate reads the certificate the chart issued, from the Secret
// the chart wrote it to.
func serverCACertificate(ctx context.Context, cfg Config, deps Deps) ([]byte, error) {
	if deps.KubeClient == nil {
		return nil, errors.New("no cluster client")
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	secret, err := deps.KubeClient.CoreV1().Secrets(ns).Get(ctx, "oberth-tls", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	certificate, ok := secret.Data["tls.crt"]
	if !ok || len(certificate) == 0 {
		return nil, errors.New("oberth-tls carries no tls.crt")
	}
	return certificate, nil
}

// clientReachableHost names the address a host-side client should use. It has
// to be one the certificate carries: a kind install issues localhost through
// certificateNamesForKind, and any other deployment names its own with
// --tls-extra-dns-name.
func clientReachableHost(deps Deps) string {
	return sshHostFromServer(deps)
}

// tokenCommandForHost proposes the platform's own secret store. The second
// return value is the command that puts the token there, which the install
// prints rather than runs: storing a credential is the operator's to do.
func tokenCommandForHost() (read string, store string) {
	switch runtime.GOOS {
	case "darwin":
		return "security find-generic-password -s oberth-token -w",
			`security add-generic-password -s oberth-token -a "$USER" -w`
	default:
		if _, err := exec.LookPath("secret-tool"); err == nil {
			return "secret-tool lookup service oberth",
				`secret-tool store --label="Oberth uplink token" service oberth`
		}
		return "pass show oberth/token", "pass insert oberth/token"
	}
}

// clientConfigRoot honours XDG_CONFIG_HOME and otherwise uses ~/.config on both
// macOS and Linux. os.UserConfigDir would return ~/Library/Application Support
// on macOS, which is right for an application and wrong for something an
// operator edits and sources from a shell profile.
func clientConfigRoot() (string, error) {
	if base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); base != "" {
		return filepath.Join(base, "oberth"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "oberth"), nil
}

// displayPath shortens a path under the home directory to ~/..., because an
// absolute path to a home directory is noise in a terminal.
func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(path, home+string(filepath.Separator)) {
		return path
	}
	return "~" + strings.TrimPrefix(path, home)
}

func printClientAccessNotes(w io.Writer, root string, choice int, storeCommand string, freshToken bool) {
	if freshToken {
		_, _ = fmt.Fprintf(w, "\nStore the bearer token above where the config expects it:\n\n    %s\n", storeCommand)
	} else {
		// No token was minted on this path, so the instruction is conditional:
		// the configuration is inert until one is there, and saying nothing
		// would leave a first command that fails for a reason nobody named.
		_, _ = fmt.Fprintf(w,
			"\nThis assumes an uplink token is already in your secret store. If not,\nregister an uplink and store what it prints:\n\n    %s\n", storeCommand)
	}
	if choice == clientAccessBoth || choice == clientAccessCLI {
		_, _ = fmt.Fprintf(w, "\nThen:  . %s\n", displayPath(filepath.Join(root, "env")))
	}
	if choice == clientAccessBoth || choice == clientAccessMCP {
		_, _ = fmt.Fprintf(w,
			"\nMCP: merge %s into your client's configuration. It uses headersHelper,\n"+
				"which Claude Code implements; a client without it needs a literal\n"+
				"\"Authorization\": \"Bearer <token>\" header instead.\n",
			displayPath(filepath.Join(root, "mcp.json")))
	}
}

// certificateNamesNotYetIssued reports the addresses a deployment has asked its
// certificate to carry that the certificate it already has does not.
//
// The TLS Secret is generated once and kept, and the template renders nothing
// when it is already present, so tls.extraDNSNames on an existing release has
// no effect until that Secret is deleted. Emitting the values silently and
// letting the operator discover it as a hostname mismatch weeks later is the
// failure this prevents: the installer holds a cluster client and the Secret is
// right there, so it can simply look.
//
// Returns nothing for a fresh install, where there is no certificate yet and
// the values are about to be honoured, and nothing when the existing
// certificate already covers the names, so a repeated install stays quiet.
func certificateNamesNotYetIssued(ctx context.Context, cfg Config, deps Deps) []string {
	if len(cfg.TLSExtraDNSNames) == 0 && len(cfg.TLSExtraIPs) == 0 {
		return nil
	}
	pemBytes, err := serverCACertificate(ctx, cfg, deps)
	if err != nil {
		return nil // no certificate yet: a fresh install gets what it asked for
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}

	var missing []string
	for _, name := range cfg.TLSExtraDNSNames {
		if !slices.Contains(certificate.DNSNames, name) {
			missing = append(missing, name)
		}
	}
	for _, address := range cfg.TLSExtraIPs {
		if !slices.ContainsFunc(certificate.IPAddresses, func(held net.IP) bool {
			return held.String() == address
		}) {
			missing = append(missing, address)
		}
	}
	return missing
}

// warnCertificateNamesWillNotTakeEffect names the remedy rather than only the
// problem. Deleting a Secret holding a private key is the operator's decision,
// so it is printed rather than performed.
func warnCertificateNamesWillNotTakeEffect(w io.Writer, cfg Config, missing []string) {
	if len(missing) == 0 {
		return
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	_, _ = fmt.Fprintf(w,
		"\nWARNING: this deployment's certificate does not cover %s, and will not\n"+
			"gain them: the TLS Secret is kept across upgrades and re-issued only when\n"+
			"absent. Clients reaching the server by those names will fail verification.\n"+
			"To re-issue, after which the TLS fingerprint changes but uplinks do not:\n\n"+
			"    kubectl delete secret -n %s oberth-tls && oberth install%s\n\n",
		strings.Join(missing, ", "), ns, reinstallFlagsFor(cfg))
}

func reinstallFlagsFor(cfg Config) string {
	var flags strings.Builder
	for _, name := range cfg.TLSExtraDNSNames {
		flags.WriteString(" --tls-extra-dns-name " + name)
	}
	for _, address := range cfg.TLSExtraIPs {
		flags.WriteString(" --tls-extra-ip " + address)
	}
	return flags.String()
}

// registerWithClaudeCode adds the server through the client's own documented
// command rather than by editing its configuration file, which is the client's
// to own and whose shape may change. Reports whether it succeeded.
//
// The JSON carries a headersHelper and no credential, so nothing sensitive
// reaches the argument list.
func registerWithClaudeCode(ctx context.Context, deps Deps, config []byte) bool {
	lookPath := deps.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if _, err := lookPath("claude"); err != nil {
		return false
	}
	server, err := claudeServerEntry(config)
	if err != nil {
		return false
	}
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	_, err = run(ctx, nil, "claude", "mcp", "add-json", "oberth", string(server), "--scope", "user")
	return err == nil
}

// claudeServerEntry unwraps the mcpServers envelope, because add-json takes the
// server object itself rather than the file a manual setup would write.
func claudeServerEntry(config []byte) ([]byte, error) {
	var document struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(config, &document); err != nil {
		return nil, err
	}
	entry, ok := document.MCPServers["oberth"]
	if !ok {
		return nil, errors.New("rendered configuration has no oberth server")
	}
	return entry, nil
}
