package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/client"
)

// Claude Code is configured the same way by `oberth install` (for the machine
// it runs on) and by `oberth client setup` (for any machine that holds the
// deployment's CA). Both go through SetupClaudeCode, which does two things:
// it makes Claude Code trust the CA, and it installs and configures the
// Oberth plugin (claude-plugin/ of this repository). The plugin carries the
// MCP server entry, so no user-scope server is registered here.
//
// The CA trust is laid out exactly as the plugin's own /oberth:connect
// (claude-plugin/scripts/trust) lays it out, so either of the two can run
// first and the plugin's session hint sees the result.

// ClaudeCodeMCPTimeoutMS is the per-server request timeout the plugin's MCP
// entry carries. run_wait and events_wait block server-side for up to 600 s;
// Claude Code's default HTTP request timeout is 60 s, which would cut every
// long wait. The extra 30 s covers the final heartbeat and the response.
const ClaudeCodeMCPTimeoutMS = 630000

// claudeCodeServerName is the name of the MCP server entry.
const claudeCodeServerName = "oberth"

const (
	// ClaudePluginName and ClaudePluginMarketplaceName are the plugin and
	// the marketplace (.claude-plugin/marketplace.json) of this repository.
	ClaudePluginName            = "oberth"
	ClaudePluginMarketplaceName = "oberthci"
	// ClaudePluginID is how Claude Code names the installed plugin.
	ClaudePluginID = ClaudePluginName + "@" + ClaudePluginMarketplaceName
	// DefaultClaudePluginMarketplace is where the marketplace is fetched
	// from unless --marketplace names a path or another git source.
	DefaultClaudePluginMarketplace = "oberthci/oberth"
)

// CA trust modes for Claude Code.
const (
	// CATrustSettings makes env.NODE_EXTRA_CA_CERTS in Claude Code's user
	// settings hold the CA; Claude Code applies it to its own process.
	CATrustSettings = "settings"
	// CATrustNone configures no CA, for a server with a publicly trusted
	// certificate.
	CATrustNone = "none"
)

// CATrustChoices is the vocabulary of `oberth client setup --ca-trust`.
var CATrustChoices = []string{CATrustSettings, CATrustNone}

// ErrClaudeCodeMissing reports that the claude command is not on PATH.
var ErrClaudeCodeMissing = errors.New("claude is not on PATH; install Claude Code first")

// ClaudeCodeSetup is what SetupClaudeCode configures.
type ClaudeCodeSetup struct {
	// URL is the MCP endpoint, ending in /mcp.
	URL string
	// CA is the deployment's CA certificate: a PEM file path or inline PEM.
	CA string
	// CATrust is one of CATrustChoices; empty selects CATrustSettings.
	CATrust string
	// Marketplace is the plugin marketplace source; empty selects
	// DefaultClaudePluginMarketplace.
	Marketplace string
	// Token returns the uplink token for the plugin's credential option, or
	// is nil when this machine has none. It is called only when the token
	// is about to be saved, and its value goes to Claude Code on standard
	// input: never into an argument list, a file or the output.
	Token func(context.Context) (string, error)
	// ReplaceToken saves Token even when the plugin already holds one.
	ReplaceToken bool
	// DryRun reports what would change and changes nothing.
	DryRun bool
}

// ClaudeCodeChange is one line of what SetupClaudeCode did or would do.
type ClaudeCodeChange struct {
	// Target names what was configured, for example the plugin.
	Target string
	// Action is "no change", "installed", "configured", "set", "manual",
	// or the same prefixed with "would be " on a dry run.
	Action string
	// Details are the individual differences, in order.
	Details []string
}

// Changed reports whether the change wrote (or on a dry run would write)
// anything.
func (change ClaudeCodeChange) Changed() bool {
	return change.Action != "no change" && change.Action != "manual"
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

// claudeCodeServer is the server object of the mcp.json record `oberth
// install` writes for MCP clients other than the plugin.
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

// SetupClaudeCode makes Claude Code trust the deployment's CA and installs
// and configures the Oberth plugin. It is idempotent: what already matches is
// reported as "no change" and is not written again.
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
	trust, pin, err := ensureClaudeCodeCATrust(paths, setup)
	if err != nil {
		problems = append(problems, err)
	}
	if trust.Target != "" {
		changes = append(changes, trust)
	}
	plugin, err := ensureClaudePlugin(ctx, deps, setup, pin)
	if err != nil {
		problems = append(problems, err)
	}
	if plugin.Target != "" {
		changes = append(changes, plugin)
	}
	if duplicate := duplicateClaudeCodeServers(paths.userConfig); duplicate.Target != "" {
		changes = append(changes, duplicate)
	}
	return changes, errors.Join(problems...)
}

// claudeCodeFiles are Claude Code's user configuration (where user-scope MCP
// servers live), its user settings, and the directory the Oberth CA is
// stored in.
type claudeCodeFiles struct {
	userConfig string
	settings   string
	store      string
}

// claudeCodePaths follows Claude Code's own layout: CLAUDE_CONFIG_DIR holds
// both files when it is set, otherwise ~/.claude.json and
// ~/.claude/settings.json. The CA store is oberth/ next to the settings.
func claudeCodePaths() (claudeCodeFiles, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return claudeCodeFiles{userConfig: filepath.Join(dir, ".claude.json"), settings: filepath.Join(dir, "settings.json"), store: filepath.Join(dir, "oberth")}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return claudeCodeFiles{}, err
	}
	return claudeCodeFiles{
		userConfig: filepath.Join(home, ".claude.json"),
		settings:   filepath.Join(home, ".claude", "settings.json"),
		store:      filepath.Join(home, ".claude", "oberth"),
	}, nil
}

// duplicateClaudeCodeServers names MCP entries called oberth that Claude Code
// holds outside the plugin. The plugin's entry is the one to use; a second
// one offers every tool twice. The file is Claude Code's own: it is only
// read here, and the operator removes the entries through its CLI.
func duplicateClaudeCodeServers(path string) ClaudeCodeChange {
	body, err := os.ReadFile(path) //nolint:gosec // G304: Claude Code's user configuration file.
	if err != nil {
		return ClaudeCodeChange{}
	}
	var document struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return ClaudeCodeChange{}
	}
	var details []string
	if _, ok := document.MCPServers[claudeCodeServerName]; ok {
		details = append(details, "user scope: run `claude mcp remove "+claudeCodeServerName+" --scope user`")
	}
	var projects []string
	for project, settings := range document.Projects {
		if _, ok := settings.MCPServers[claudeCodeServerName]; ok {
			projects = append(projects, project)
		}
	}
	sort.Strings(projects)
	for _, project := range projects {
		details = append(details, "local scope of "+project+": run `claude mcp remove "+claudeCodeServerName+" --scope local` there")
	}
	if len(details) == 0 {
		return ClaudeCodeChange{}
	}
	return ClaudeCodeChange{
		Target:  "Claude Code MCP entries named " + claudeCodeServerName + " besides the plugin's (" + displayPath(path) + ")",
		Action:  "manual",
		Details: details,
	}
}

// pluginRunner runs the claude CLI.
type pluginRunner func(ctx context.Context, input []byte, args ...string) ([]byte, error)

// ensureClaudePlugin installs the plugin when it is missing, enables it when
// it is disabled, and saves the options that differ: the MCP URL, the CA pin
// and, when this machine has one, the token.
func ensureClaudePlugin(ctx context.Context, deps Deps, setup ClaudeCodeSetup, pin string) (ClaudeCodeChange, error) {
	change := ClaudeCodeChange{Target: "Claude Code plugin " + ClaudePluginID, Action: "no change"}
	marketplace := setup.Marketplace
	if marketplace == "" {
		marketplace = DefaultClaudePluginMarketplace
	}
	want := map[string]string{"url": setup.URL}
	if pin != "" {
		want["ca_pin"] = pin
	}
	tokenHint := "token: not saved; enter it with /plugin configure " + ClaudePluginID + " in Claude Code"
	if setup.DryRun {
		change.Action = "would be installed or configured"
		change.Details = []string{
			"when missing, run: claude plugin install " + ClaudePluginName + " --marketplace " + marketplace,
			"url: " + setup.URL,
		}
		if pin != "" {
			change.Details = append(change.Details, "ca_pin: "+pin)
		}
		if setup.Token != nil {
			change.Details = append(change.Details, "token: saved in Claude Code's credential store when the plugin holds none")
		} else {
			change.Details = append(change.Details, tokenHint)
		}
		return change, nil
	}
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	claude := func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		out, err := run(ctx, input, "claude", args...)
		if err != nil {
			return out, fmt.Errorf("claude %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}

	installed, enabled, err := claudePluginState(ctx, claude)
	if err != nil {
		return ClaudeCodeChange{}, err
	}
	if !installed {
		if _, err := claude(ctx, nil, "plugin", "install", ClaudePluginName, "--marketplace", marketplace); err != nil {
			return ClaudeCodeChange{}, err
		}
		change.Action = "installed"
		change.Details = append(change.Details, "marketplace: "+marketplace)
	} else if !enabled {
		if _, err := claude(ctx, nil, "plugin", "enable", ClaudePluginID); err != nil {
			return ClaudeCodeChange{}, err
		}
		change.Action = "configured"
		change.Details = append(change.Details, "enabled")
	}

	inputs, configured, err := claudePluginOptions(ctx, claude)
	if err != nil {
		return change, err
	}
	values := map[string]string{}
	for _, key := range []string{"url", "ca_pin"} {
		if value, ok := want[key]; ok && inputs[key] != value {
			values[key] = value
			change.Details = append(change.Details, key+": "+value)
		}
	}
	switch {
	case setup.Token != nil && (setup.ReplaceToken || !configured["token"]):
		token, err := setup.Token(ctx)
		if err != nil {
			change.Details = append(change.Details, tokenHint+" ("+err.Error()+")")
			break
		}
		values["token"] = token
		change.Details = append(change.Details, "token: saved in Claude Code's credential store")
	case !configured["token"]:
		change.Details = append(change.Details, tokenHint)
	}
	if len(values) > 0 {
		// Standard input, so the token reaches neither an argument list nor
		// a file of ours.
		payload, err := json.Marshal(values)
		if err != nil {
			return change, err
		}
		_, err = claude(ctx, payload, "plugin", "configure", ClaudePluginID, "--values-stdin")
		clear(payload)
		if err != nil {
			return change, err
		}
		if change.Action == "no change" {
			change.Action = "configured"
		}
	}
	return change, nil
}

// claudePluginState reads `claude plugin list --json`.
func claudePluginState(ctx context.Context, claude pluginRunner) (installed, enabled bool, err error) {
	out, err := claude(ctx, nil, "plugin", "list", "--json")
	if err != nil {
		return false, false, err
	}
	var plugins []struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(out, &plugins); err != nil {
		return false, false, fmt.Errorf("claude plugin list --json: %w", err)
	}
	for _, plugin := range plugins {
		if plugin.ID == ClaudePluginID {
			return true, plugin.Enabled, nil
		}
	}
	return false, false, nil
}

// claudePluginOptions reads `claude plugin configure --json`: the saved
// values of the options that are not sensitive, and which options are set.
func claudePluginOptions(ctx context.Context, claude pluginRunner) (map[string]string, map[string]bool, error) {
	out, err := claude(ctx, nil, "plugin", "configure", ClaudePluginID, "--json")
	if err != nil {
		return nil, nil, err
	}
	var options struct {
		Inputs     map[string]any `json:"inputs"`
		Configured []string       `json:"configured"`
	}
	if err := json.Unmarshal(out, &options); err != nil {
		return nil, nil, fmt.Errorf("claude plugin configure --json: %w", err)
	}
	inputs := map[string]string{}
	for key, value := range options.Inputs {
		if text, ok := value.(string); ok {
			inputs[key] = text
		}
	}
	configured := map[string]bool{}
	for _, key := range options.Configured {
		configured[key] = true
	}
	return inputs, configured, nil
}

func ensureClaudeCodeCATrust(paths claudeCodeFiles, setup ClaudeCodeSetup) (ClaudeCodeChange, string, error) {
	mode := setup.CATrust
	if mode == "" {
		mode = CATrustSettings
	}
	switch mode {
	case CATrustNone:
		return ClaudeCodeChange{}, "", nil
	case CATrustSettings:
	default:
		return ClaudeCodeChange{}, "", fmt.Errorf("--ca-trust must be one of %s", strings.Join(CATrustChoices, ", "))
	}
	if setup.CA == "" {
		return ClaudeCodeChange{}, "", errors.New("no CA certificate: pass --ca-file, or --ca-trust none for a publicly trusted server certificate")
	}
	body, authority, err := readCACertificates(setup.CA)
	if err != nil {
		return ClaudeCodeChange{}, "", err
	}
	pin := CertificatePin(authority[0])
	change, err := ensureSettingsCATrust(paths, body, authority, pin, setup.DryRun)
	return change, pin, err
}

// CertificatePin is the SHA-256 fingerprint of a certificate, in lowercase
// hex: what `oberth client pin` prints and /oberth:connect compares.
func CertificatePin(certificate *x509.Certificate) string {
	sum := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(sum[:])
}

// CAPin returns the pin of the first certificate of a CA: a PEM file path
// or inline PEM.
func CAPin(authority string) (string, error) {
	_, certificates, err := readCACertificates(authority)
	if err != nil {
		return "", err
	}
	return CertificatePin(certificates[0]), nil
}

// readCACertificates reads a CA given as a PEM file path or as inline PEM,
// and returns its certificates re-encoded and parsed.
func readCACertificates(authority string) ([]byte, []*x509.Certificate, error) {
	body := []byte(authority)
	name := "the inline CA certificate"
	if !strings.Contains(authority, "-----BEGIN") {
		var err error
		body, err = os.ReadFile(authority) //nolint:gosec // G304: the operator's CA file.
		if err != nil {
			return nil, nil, fmt.Errorf("CA certificate: %w", err)
		}
		name = "CA certificate " + displayPath(authority)
	}
	certificates := parsePEMCertificates(body)
	if len(certificates) == 0 {
		return nil, nil, fmt.Errorf("%s holds no PEM certificate", name)
	}
	var encoded bytes.Buffer
	for _, certificate := range certificates {
		_ = pem.Encode(&encoded, &pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	}
	return encoded.Bytes(), certificates, nil
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

// ensureSettingsCATrust stores the CA as oberth/ca.crt (with its pin as
// oberth/ca.sha256) next to Claude Code's user settings and makes
// env.NODE_EXTRA_CA_CERTS hold it. Every other key of the settings, and the
// order of the keys, is preserved.
//
// NODE_EXTRA_CA_CERTS names a single file. One that already names another
// bundle is kept when that bundle holds the CA; otherwise oberth/bundle.pem
// becomes a copy of that bundle plus the CA, its origin is recorded in
// oberth/bundle.source, and the setting names the copy. The operating
// system's trust store is never touched.
func ensureSettingsCATrust(paths claudeCodeFiles, authorityPEM []byte, authority []*x509.Certificate, pin string, dryRun bool) (ClaudeCodeChange, error) {
	const variable = "NODE_EXTRA_CA_CERTS"
	path := paths.settings
	stored := filepath.Join(paths.store, "ca.crt")
	bundle := filepath.Join(paths.store, "bundle.pem")
	bundleSource := filepath.Join(paths.store, "bundle.source")
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

	// What the setting should name, and the files that back it.
	want := stored
	files := map[string][]byte{stored: authorityPEM, filepath.Join(paths.store, "ca.sha256"): []byte(pin + "\n")}
	var details []string
	origin := ""
	switch current {
	case "", stored:
	case bundle:
		want = bundle
		if recorded, readErr := os.ReadFile(bundleSource); readErr == nil { //nolint:gosec // G304: our own record next to the settings.
			origin = strings.TrimSpace(string(recorded))
		}
	default:
		held, readErr := os.ReadFile(current) //nolint:gosec // G304: the bundle the operator's settings already name.
		switch {
		case readErr == nil && containsAll(parsePEMCertificates(held), authority):
			want = current
			details = append(details, "kept "+current+", which already holds the Oberth CA")
		case readErr != nil:
			return ClaudeCodeChange{}, fmt.Errorf("%s env.%s names %s, which cannot be read; repair or remove that setting first", displayPath(path), variable, current)
		default:
			want, origin = bundle, current
		}
	}
	if want == bundle {
		held, readErr := os.ReadFile(origin) //nolint:gosec // G304: the bundle the operator's settings named.
		if origin == "" || readErr != nil {
			return ClaudeCodeChange{}, fmt.Errorf("%s was built from a file that cannot be read any more; repair env.%s in %s first", displayPath(bundle), variable, displayPath(path))
		}
		files[bundle] = append(append(bytes.TrimRight(held, "\n"), '\n'), authorityPEM...)
		files[bundleSource] = []byte(origin + "\n")
		details = append(details, displayPath(bundle)+" holds a copy of "+origin+" and the Oberth CA; run this again when "+origin+" changes")
	}

	change := ClaudeCodeChange{Target: target, Action: "no change", Details: details}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if held, readErr := os.ReadFile(name); readErr == nil && bytes.Equal(held, files[name]) { //nolint:gosec // G304: our own files next to the settings.
			delete(files, name)
			continue
		}
		change.Action = "set"
		change.Details = append(change.Details, "wrote "+displayPath(name))
	}
	var merged []byte
	if current != want {
		value, err := json.Marshal(want)
		if err != nil {
			return ClaudeCodeChange{}, err
		}
		environment.set(variable, value)
		encodedEnvironment, err := environment.marshal()
		if err != nil {
			return ClaudeCodeChange{}, err
		}
		settings.set("env", encodedEnvironment)
		if merged, err = settings.marshal(); err != nil {
			return ClaudeCodeChange{}, err
		}
		shown := current
		if shown == "" {
			shown = "unset"
		}
		change.Action = "set"
		change.Details = append(change.Details, variable+": "+shown+" -> "+want)
	}
	if change.Action == "no change" {
		return change, nil
	}
	if dryRun {
		change.Action = "would be set"
		return change, nil
	}
	if err := os.MkdirAll(paths.store, 0o700); err != nil {
		return ClaudeCodeChange{}, err
	}
	for _, name := range names {
		content, ok := files[name]
		if !ok {
			continue
		}
		if err := atomicWriteFile(name, content, 0o644); err != nil { //nolint:gosec // G306: public certificates and their fingerprint.
			return ClaudeCodeChange{}, err
		}
	}
	if merged != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return ClaudeCodeChange{}, err
		}
		if err := atomicWriteFile(path, merged, mode); err != nil {
			return ClaudeCodeChange{}, err
		}
	}
	change.Details = append(change.Details, "restart Claude Code once: it reads its CA trust only when it starts")
	return change, nil
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
