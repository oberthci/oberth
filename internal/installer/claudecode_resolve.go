package installer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/client"
)

// ClientSetupFlags are `oberth client setup`'s flags. Every empty field falls
// back to the CLI's existing configuration on this machine.
type ClientSetupFlags struct {
	URL          string
	CAFile       string
	TokenFile    string
	TokenCommand string
	CATrust      string
	DryRun       bool
}

// ResolveClaudeCodeSetup turns the flags into a ClaudeCodeSetup, filling every
// value the operator did not pass from what the CLI already uses, in order:
// flags, the environment (OBERTH_MCP_URL, OBERTH_BASE_URL, OBERTH_CA_CERT,
// OBERTH_TOKEN_COMMAND), the env and mcp.json files `oberth install` writes,
// the CA and token files in the client configuration directory, and for the
// URL the server Claude Code already has registered.
//
// The token itself is never read here. Only the command that prints it is
// passed on, and a literal OBERTH_TOKEN is deliberately ignored: it would
// have to be written into Claude Code's configuration to be used.
func ResolveClaudeCodeSetup(flags ClientSetupFlags) (ClaudeCodeSetup, []string, error) {
	var notes []string
	setup := ClaudeCodeSetup{CATrust: flags.CATrust, DryRun: flags.DryRun}
	if setup.CATrust == "" {
		setup.CATrust = CATrustSettings
	}
	if !validCATrust(setup.CATrust) {
		return setup, nil, fmt.Errorf("--ca-trust must be one of %s", strings.Join(CATrustChoices, ", "))
	}
	root, err := clientConfigRoot()
	if err != nil {
		return setup, nil, err
	}
	environment := client.FromEnv()
	written := readClientEnvFile(filepath.Join(root, "env"))

	// URL.
	candidates := []string{
		flags.URL,
		strings.TrimSpace(os.Getenv("OBERTH_MCP_URL")),
		environment.BaseURL,
		written["OBERTH_BASE_URL"],
		mcpConfigURL(filepath.Join(root, "mcp.json")),
	}
	if paths, pathErr := claudeCodePaths(); pathErr == nil {
		if existing, _, readErr := readClaudeCodeServer(paths.userConfig); readErr == nil && existing != nil {
			if registered, _ := existing["url"].(string); registered != "" {
				candidates = append(candidates, registered)
			}
		}
	}
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			setup.URL, err = mcpEndpoint(candidate)
			if err != nil {
				return setup, nil, err
			}
			break
		}
	}
	if setup.URL == "" {
		return setup, nil, errors.New("no server address: pass --url https://<host>:<port>/mcp, or set OBERTH_BASE_URL")
	}

	// CA.
	for _, candidate := range []string{flags.CAFile, environment.CACert, written["OBERTH_CA_CERT"], existingFile(filepath.Join(root, "ca.crt"))} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			setup.CAFile, err = filepath.Abs(candidate)
			if err != nil {
				return setup, nil, err
			}
			break
		}
	}

	// Token source.
	switch {
	case flags.TokenCommand != "" && flags.TokenFile != "":
		return setup, nil, errors.New("pass --token-command or --token-file, not both")
	case flags.TokenCommand != "":
		setup.TokenCommand, setup.ExplicitToken = flags.TokenCommand, true
	case flags.TokenFile != "":
		path, err := ownerOnlyTokenFile(flags.TokenFile)
		if err != nil {
			return setup, nil, err
		}
		setup.TokenCommand, setup.ExplicitToken = "cat "+shellQuote(path), true
	case environment.TokenCommand != "":
		setup.TokenCommand = environment.TokenCommand
	case written["OBERTH_TOKEN_COMMAND"] != "":
		setup.TokenCommand = written["OBERTH_TOKEN_COMMAND"]
	case existingFile(filepath.Join(root, "token")) != "":
		if _, err := ownerOnlyTokenFile(filepath.Join(root, "token")); err != nil {
			return setup, nil, err
		}
		setup.TokenCommand = tokenFileReadCommand
	default:
		read, store := tokenCommandForHost()
		setup.TokenCommand = read
		notes = append(notes, "the token is read with `"+read+"`; store it there first if it is not yet: "+store)
	}
	if environment.Token != "" {
		notes = append(notes, "OBERTH_TOKEN is set but not used: Claude Code reads the token through the headersHelper command instead")
	}
	return setup, notes, nil
}

func validCATrust(value string) bool {
	for _, choice := range CATrustChoices {
		if value == choice {
			return true
		}
	}
	return false
}

// mcpEndpoint accepts either the server's base address or its /mcp endpoint.
func mcpEndpoint(address string) (string, error) {
	parsed, err := url.Parse(address)
	if err != nil {
		return "", fmt.Errorf("server address %q: %w", address, err)
	}
	if path := strings.TrimRight(parsed.Path, "/"); path == "" {
		parsed.Path = "/mcp"
	}
	endpoint := parsed.String()
	if _, err := client.ValidateServerURL(endpoint); err != nil {
		return "", err
	}
	return endpoint, nil
}

func existingFile(path string) string {
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path
	}
	return ""
}

// ownerOnlyTokenFile applies the same rule as `oberth mcp --token-file`: a
// token readable by anyone else is refused.
func ownerOnlyTokenFile(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("token file %s has mode %04o; expected 0600 (no group/other access)", displayPath(absolute), perm)
	}
	return absolute, nil
}

// readClientEnvFile reads the export lines renderClientEnv writes.
func readClientEnvFile(path string) map[string]string {
	values := map[string]string{}
	body, err := os.ReadFile(path) //nolint:gosec // G304: the client configuration this installer writes.
	if err != nil {
		return values
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		name, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok || strings.HasPrefix(line, "#") || !strings.HasPrefix(name, "OBERTH_") {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		values[name] = strings.TrimSpace(value)
	}
	return values
}

// mcpConfigURL reads the endpoint from the mcp.json `oberth install` writes.
func mcpConfigURL(path string) string {
	body, err := os.ReadFile(path) //nolint:gosec // G304: the client configuration this installer writes.
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return ""
	}
	var document struct {
		MCPServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(body, &document) != nil {
		return ""
	}
	return document.MCPServers[claudeCodeServerName].URL
}
