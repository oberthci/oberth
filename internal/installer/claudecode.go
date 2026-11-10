package installer

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/client"
)

// Claude Code is configured the same way by `oberth install` (for the machine
// it runs on) and by `oberth client setup` (for any machine that holds a token
// and the deployment's CA). Both paths go through SetupClaudeCode, so the
// registered entry, the timeout and the CA trust cannot drift between them.
//
// Nothing here writes a credential. The registered server names a
// headersHelper command that reads the token when Claude Code connects, and
// the CA is a public certificate.

// ClaudeCodeMCPTimeoutMS is the per-server request timeout registered with
// Claude Code. run_wait blocks server-side for up to 600 s; Claude Code's
// default HTTP request timeout is 60 s, which would cut every long wait. The
// extra 30 s covers the final heartbeat and the response itself.
const ClaudeCodeMCPTimeoutMS = 630000

// claudeCodeServerName is the name the server is registered under.
const claudeCodeServerName = "oberth"

// CA trust modes for Claude Code.
const (
	// CATrustSettings sets env.NODE_EXTRA_CA_CERTS in Claude Code's user
	// settings, which Claude Code applies to its own process.
	CATrustSettings = "settings"
	// CATrustSystem relies on the operating system's trust store, which
	// Claude Code's native binary reads by default.
	CATrustSystem = "system"
	// CATrustNone configures no CA, for a server with a publicly trusted
	// certificate.
	CATrustNone = "none"
)

// CATrustChoices is the vocabulary of `oberth client setup --ca-trust`.
var CATrustChoices = []string{CATrustSettings, CATrustSystem, CATrustNone}

// ErrClaudeCodeMissing reports that the claude command is not on PATH.
var ErrClaudeCodeMissing = errors.New("claude is not on PATH; install Claude Code first")

// ClaudeCodeSetup is what SetupClaudeCode configures.
type ClaudeCodeSetup struct {
	// URL is the MCP endpoint, ending in /mcp.
	URL string
	// TokenCommand prints the bearer token; the headersHelper runs it.
	TokenCommand string
	// ExplicitToken replaces an existing headersHelper with TokenCommand.
	// Without it a working helper the operator already registered is kept,
	// because only the operator knows where their token lives.
	ExplicitToken bool
	// CAFile is the absolute path of the deployment's CA certificate.
	CAFile string
	// CATrust is one of CATrustChoices; empty selects CATrustSettings.
	CATrust string
	// DryRun reports what would change and changes nothing.
	DryRun bool
}

// ClaudeCodeChange is one line of what SetupClaudeCode did or would do.
type ClaudeCodeChange struct {
	// Target names what was configured, for example the MCP server.
	Target string
	// Action is "no change", "registered", "updated", "set", "manual",
	// or the same prefixed with "would " on a dry run.
	Action string
	// Details are the individual differences, in order.
	Details []string
}

// Changed reports whether the change wrote (or on a dry run would write)
// anything.
func (change ClaudeCodeChange) Changed() bool {
	return change.Action != "no change"
}

// String renders the change as the lines `oberth client setup` prints.
func (change ClaudeCodeChange) String() string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s: %s\n", change.Target, change.Action)
	for _, detail := range change.Details {
		fmt.Fprintf(&out, "    %s\n", detail)
	}
	return out.String()
}

// claudeCodeServer is the server object `claude mcp add-json` takes.
type claudeCodeServer struct {
	Type          string `json:"type"`
	URL           string `json:"url"`
	HeadersHelper string `json:"headersHelper"`
	Timeout       int    `json:"timeout"`
}

func newClaudeCodeServer(mcpURL, headersHelper string) claudeCodeServer {
	return claudeCodeServer{Type: "http", URL: mcpURL, HeadersHelper: headersHelper, Timeout: ClaudeCodeMCPTimeoutMS}
}

// headersHelperFor wraps a token command in the JSON Claude Code expects from
// a headersHelper.
func headersHelperFor(tokenCommand string) string {
	return fmt.Sprintf(`printf '{"Authorization":"Bearer %%s"}' "$(%s)"`, tokenCommand)
}

// SetupClaudeCode registers the user-scope MCP server and ensures Claude Code
// trusts the deployment's CA. It is idempotent: a configuration that already
// matches is reported as "no change" and nothing is run or written.
//
// Both steps run even when one fails; the errors are joined.
func SetupClaudeCode(ctx context.Context, deps Deps, setup ClaudeCodeSetup) ([]ClaudeCodeChange, error) {
	lookPath := deps.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if _, err := lookPath("claude"); err != nil {
		return nil, ErrClaudeCodeMissing
	}
	if _, err := client.ValidateServerURL(setup.URL); err != nil {
		return nil, err
	}
	paths, err := claudeCodePaths()
	if err != nil {
		return nil, err
	}

	var changes []ClaudeCodeChange
	var problems []error
	registration, err := ensureClaudeCodeServer(ctx, deps, paths, setup)
	if err != nil {
		problems = append(problems, err)
	}
	changes = append(changes, registration...)
	trust, err := ensureClaudeCodeCATrust(paths, setup)
	if err != nil {
		problems = append(problems, err)
	}
	if trust.Target != "" {
		changes = append(changes, trust)
	}
	return changes, errors.Join(problems...)
}

// claudeCodeFiles are Claude Code's user configuration (where user-scope MCP
// servers live) and its user settings.
type claudeCodeFiles struct {
	userConfig string
	settings   string
}

// claudeCodePaths follows Claude Code's own layout: CLAUDE_CONFIG_DIR holds
// both files when it is set, otherwise ~/.claude.json and
// ~/.claude/settings.json.
func claudeCodePaths() (claudeCodeFiles, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return claudeCodeFiles{userConfig: filepath.Join(dir, ".claude.json"), settings: filepath.Join(dir, "settings.json")}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return claudeCodeFiles{}, err
	}
	return claudeCodeFiles{userConfig: filepath.Join(home, ".claude.json"), settings: filepath.Join(home, ".claude", "settings.json")}, nil
}

// literalCredential matches a bearer token written out in a configuration:
// a header or helper that embeds one is replaced, never kept.
var literalCredential = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}|oberth_[A-Za-z0-9]{16,}`)

func ensureClaudeCodeServer(ctx context.Context, deps Deps, paths claudeCodeFiles, setup ClaudeCodeSetup) ([]ClaudeCodeChange, error) {
	target := "Claude Code MCP server " + claudeCodeServerName + " (user scope, " + displayPath(paths.userConfig) + ")"
	existing, shadows, err := readClaudeCodeServer(paths.userConfig)
	if err != nil {
		return nil, err
	}
	helper := headersHelperFor(setup.TokenCommand)
	if existing != nil && !setup.ExplicitToken {
		if kept, _ := existing["headersHelper"].(string); strings.TrimSpace(kept) != "" && !literalCredential.MatchString(kept) {
			helper = kept
		}
	}
	want := newClaudeCodeServer(setup.URL, helper)
	differences := claudeCodeServerDifferences(existing, want)

	var changes []ClaudeCodeChange
	if len(differences) == 0 {
		changes = append(changes, ClaudeCodeChange{Target: target, Action: "no change"})
	} else {
		entry, err := json.Marshal(want)
		if err != nil {
			return nil, err
		}
		action := "registered"
		if existing != nil {
			action = "updated"
		}
		change := ClaudeCodeChange{Target: target, Action: action, Details: differences}
		if setup.DryRun {
			change.Action = "would be " + action
			if existing != nil {
				change.Details = append(change.Details, "run: claude mcp remove "+claudeCodeServerName+" --scope user")
			}
			change.Details = append(change.Details, "run: claude mcp add-json "+claudeCodeServerName+" "+shellQuote(string(entry))+" --scope user")
		} else if err := registerWithClaudeCode(ctx, deps, entry, existing != nil); err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	if len(shadows) > 0 {
		changes = append(changes, ClaudeCodeChange{
			Target:  "Claude Code local-scope " + claudeCodeServerName + " entries",
			Action:  "manual",
			Details: append([]string{"these projects register their own " + claudeCodeServerName + " server, which takes precedence there; remove it with `claude mcp remove " + claudeCodeServerName + " --scope local` in each:"}, shadows...),
		})
	}
	return changes, nil
}

// readClaudeCodeServer returns the user-scope server entry, if any, and the
// projects whose local-scope entry of the same name overrides it. The file is
// Claude Code's own; it is only read here, and changed through its CLI.
func readClaudeCodeServer(path string) (map[string]any, []string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: Claude Code's user configuration file.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var document struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", displayPath(path), err)
	}
	var shadows []string
	for project, settings := range document.Projects {
		if _, ok := settings.MCPServers[claudeCodeServerName]; ok {
			shadows = append(shadows, project)
		}
	}
	sort.Strings(shadows)
	return document.MCPServers[claudeCodeServerName], shadows, nil
}

// claudeCodeServerDifferences lists what registering want would change. A
// longer timeout than ours is the operator's and is kept.
func claudeCodeServerDifferences(existing map[string]any, want claudeCodeServer) []string {
	if existing == nil {
		return []string{
			"type: http",
			"url: " + want.URL,
			"headersHelper: " + want.HeadersHelper,
			"timeout: " + strconv.Itoa(want.Timeout) + " ms",
		}
	}
	var differences []string
	if value, _ := existing["type"].(string); value != want.Type {
		differences = append(differences, fmt.Sprintf("type: %s -> %s", orUnset(value), want.Type))
	}
	if value, _ := existing["url"].(string); value != want.URL {
		differences = append(differences, fmt.Sprintf("url: %s -> %s", orUnset(value), want.URL))
	}
	if headers, ok := existing["headers"]; ok {
		description := "headers: removed (the headersHelper supplies Authorization)"
		if encoded, _ := json.Marshal(headers); literalCredential.Match(encoded) {
			description = "headers: removed a literal bearer token (the headersHelper supplies Authorization)"
		}
		differences = append(differences, description)
	}
	if value, _ := existing["headersHelper"].(string); value != want.HeadersHelper {
		shown := value
		if literalCredential.MatchString(value) {
			shown = "<helper with a literal token>"
		}
		differences = append(differences, fmt.Sprintf("headersHelper: %s -> %s", orUnset(shown), want.HeadersHelper))
	}
	if value, ok := existing["timeout"].(float64); !ok || value < float64(want.Timeout) {
		shown := "unset (Claude Code default 60000 ms)"
		if ok {
			shown = strconv.FormatFloat(value, 'f', -1, 64) + " ms"
		}
		differences = append(differences, fmt.Sprintf("timeout: %s -> %d ms", shown, want.Timeout))
	}
	return differences
}

func orUnset(value string) string {
	if value == "" {
		return "unset"
	}
	return value
}

// registerWithClaudeCode adds the server through the client's own documented
// command rather than by editing its configuration file, which is the client's
// to own and whose shape may change. add-json refuses an existing name, so a
// stale entry is removed first.
//
// The JSON carries a headersHelper and no credential, so nothing sensitive
// reaches the argument list.
func registerWithClaudeCode(ctx context.Context, deps Deps, server []byte, replace bool) error {
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	if replace {
		if out, err := run(ctx, nil, "claude", "mcp", "remove", claudeCodeServerName, "--scope", "user"); err != nil {
			return fmt.Errorf("claude mcp remove %s: %w: %s", claudeCodeServerName, err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := run(ctx, nil, "claude", "mcp", "add-json", claudeCodeServerName, string(server), "--scope", "user"); err != nil {
		return fmt.Errorf("claude mcp add-json %s: %w: %s", claudeCodeServerName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ensureClaudeCodeCATrust(paths claudeCodeFiles, setup ClaudeCodeSetup) (ClaudeCodeChange, error) {
	mode := setup.CATrust
	if mode == "" {
		mode = CATrustSettings
	}
	if mode == CATrustNone {
		return ClaudeCodeChange{}, nil
	}
	if setup.CAFile == "" {
		return ClaudeCodeChange{}, errors.New("no CA certificate: pass --ca-file, or --ca-trust none for a publicly trusted server certificate")
	}
	authority, err := readCACertificates(setup.CAFile)
	if err != nil {
		return ClaudeCodeChange{}, err
	}
	switch mode {
	case CATrustSettings:
		return ensureSettingsCATrust(paths.settings, setup.CAFile, authority, setup.DryRun)
	case CATrustSystem:
		return systemCATrust(setup.CAFile, authority), nil
	default:
		return ClaudeCodeChange{}, fmt.Errorf("--ca-trust must be one of %s", strings.Join(CATrustChoices, ", "))
	}
}

func readCACertificates(path string) ([]*x509.Certificate, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: the operator's CA file.
	if err != nil {
		return nil, fmt.Errorf("CA certificate: %w", err)
	}
	certificates := parsePEMCertificates(body)
	if len(certificates) == 0 {
		return nil, fmt.Errorf("CA certificate %s holds no PEM certificate", displayPath(path))
	}
	return certificates, nil
}

func parsePEMCertificates(body []byte) []*x509.Certificate {
	var certificates []*x509.Certificate
	for {
		var block *pem.Block
		block, body = pem.Decode(body)
		if block == nil {
			return certificates
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if certificate, err := x509.ParseCertificate(block.Bytes); err == nil {
			certificates = append(certificates, certificate)
		}
	}
}

// containsAll reports whether every certificate in want is in bundle.
func containsAll(bundle, want []*x509.Certificate) bool {
	for _, certificate := range want {
		found := false
		for _, held := range bundle {
			if held.Equal(certificate) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ensureSettingsCATrust merges env.NODE_EXTRA_CA_CERTS into Claude Code's
// user settings. Every other key, and the order of the keys, is preserved.
// NODE_EXTRA_CA_CERTS names a single file, so one that already names a
// different bundle is left alone: it is kept when it holds our CA, and
// reported as a conflict when it does not.
func ensureSettingsCATrust(path, caFile string, authority []*x509.Certificate, dryRun bool) (ClaudeCodeChange, error) {
	const variable = "NODE_EXTRA_CA_CERTS"
	target := "Claude Code CA trust (" + displayPath(path) + " env." + variable + ")"
	body, err := os.ReadFile(path) //nolint:gosec // G304: Claude Code's user settings file.
	mode := os.FileMode(0o600)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		body = nil
	case err != nil:
		return ClaudeCodeChange{}, err
	default:
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		}
	}
	settings, err := parseJSONObject(body)
	if err != nil {
		return ClaudeCodeChange{}, fmt.Errorf("read %s: %w", displayPath(path), err)
	}
	environment, err := parseJSONObject(settings.values["env"])
	if err != nil {
		return ClaudeCodeChange{}, fmt.Errorf("read %s env: %w", displayPath(path), err)
	}
	var current string
	if raw, ok := environment.values[variable]; ok {
		if err := json.Unmarshal(raw, &current); err != nil {
			return ClaudeCodeChange{}, fmt.Errorf("read %s env.%s: %w", displayPath(path), variable, err)
		}
	}
	if current == caFile {
		return ClaudeCodeChange{Target: target, Action: "no change"}, nil
	}
	if current != "" {
		bundle, readErr := os.ReadFile(current) //nolint:gosec // G304: the bundle the operator's settings already name.
		if readErr == nil && containsAll(parsePEMCertificates(bundle), authority) {
			return ClaudeCodeChange{Target: target, Action: "no change", Details: []string{"kept " + current + ", which already holds the Oberth CA"}}, nil
		}
		return ClaudeCodeChange{}, fmt.Errorf("%s env.%s already names %s, which does not hold the Oberth CA; append %s to that bundle, or rerun with --ca-trust system", displayPath(path), variable, current, caFile)
	}

	value, err := json.Marshal(caFile)
	if err != nil {
		return ClaudeCodeChange{}, err
	}
	environment.set(variable, value)
	encodedEnvironment, err := environment.marshal()
	if err != nil {
		return ClaudeCodeChange{}, err
	}
	settings.set("env", encodedEnvironment)
	merged, err := settings.marshal()
	if err != nil {
		return ClaudeCodeChange{}, err
	}
	change := ClaudeCodeChange{Target: target, Action: "set", Details: []string{variable + ": unset -> " + caFile}}
	if dryRun {
		change.Action = "would be set"
		return change, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ClaudeCodeChange{}, err
	}
	if err := atomicWriteFile(path, merged, mode); err != nil {
		return ClaudeCodeChange{}, err
	}
	return change, nil
}

// systemCATrust reports whether the operating system already trusts the CA
// and, when it does not, prints the commands that make it. Changing the
// system trust store needs root, so it stays the operator's step.
func systemCATrust(caFile string, authority []*x509.Certificate) ClaudeCodeChange {
	target := "Claude Code CA trust (operating system trust store)"
	if systemTrusts(authority) {
		return ClaudeCodeChange{Target: target, Action: "no change"}
	}
	return ClaudeCodeChange{Target: target, Action: "manual", Details: systemTrustCommands(caFile)}
}

func systemTrusts(authority []*x509.Certificate) bool {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return false
	}
	for _, certificate := range authority {
		if _, err := certificate.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
			return false
		}
	}
	return true
}

func systemTrustCommands(caFile string) []string {
	quoted := shellQuote(caFile)
	if runtime.GOOS == "darwin" {
		return []string{"run: sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain " + quoted}
	}
	if _, err := exec.LookPath("update-ca-trust"); err == nil {
		return []string{"run: sudo install -m 0644 " + quoted + " /etc/pki/ca-trust/source/anchors/oberth.crt && sudo update-ca-trust"}
	}
	return []string{"run: sudo install -m 0644 " + quoted + " /usr/local/share/ca-certificates/oberth.crt && sudo update-ca-certificates"}
}

// shellQuote quotes a value for a POSIX shell.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// VerifyClaudeCode asks Claude Code for the server's status, which runs the
// headersHelper and connects, and returns the status line.
func VerifyClaudeCode(ctx context.Context, deps Deps) (string, error) {
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	out, err := run(ctx, nil, "claude", "mcp", "get", claudeCodeServerName)
	status := ""
	for line := range strings.SplitSeq(string(out), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "Status:") {
			status = trimmed
		}
	}
	if err != nil {
		return status, fmt.Errorf("claude mcp get %s: %w: %s", claudeCodeServerName, err, strings.TrimSpace(string(out)))
	}
	if status == "" {
		return "", fmt.Errorf("claude mcp get %s printed no status", claudeCodeServerName)
	}
	if !strings.Contains(status, "Connected") {
		return status, fmt.Errorf("claude mcp get %s: %s", claudeCodeServerName, status)
	}
	return status, nil
}

// jsonObject is a JSON object that keeps its keys in their original order, so
// a merged settings file differs from the original only where it was changed.
type jsonObject struct {
	keys   []string
	values map[string]json.RawMessage
}

func parseJSONObject(data []byte) (*jsonObject, error) {
	object := &jsonObject{values: map[string]json.RawMessage{}}
	if len(bytes.TrimSpace(data)) == 0 {
		return object, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("not a JSON object")
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("object key is not a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		object.set(key, value)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON object")
	}
	return object, nil
}

func (object *jsonObject) set(key string, value json.RawMessage) {
	if _, ok := object.values[key]; !ok {
		object.keys = append(object.keys, key)
	}
	object.values[key] = value
}

func (object *jsonObject) marshal() ([]byte, error) {
	var compact bytes.Buffer
	compact.WriteByte('{')
	for index, key := range object.keys {
		if index > 0 {
			compact.WriteByte(',')
		}
		encoded, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		compact.Write(encoded)
		compact.WriteByte(':')
		if err := json.Compact(&compact, object.values[key]); err != nil {
			return nil, err
		}
	}
	compact.WriteByte('}')
	var indented bytes.Buffer
	if err := json.Indent(&indented, compact.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	indented.WriteByte('\n')
	return indented.Bytes(), nil
}
