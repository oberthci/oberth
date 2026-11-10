package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ConnectionFlags are the transport flags every remote command accepts:
// --url (the server's base address or its /mcp endpoint), --token-file and
// --ca-file. Empty fields fall back to the environment, then to the client
// files oberth install writes (ConfigDir).
type ConnectionFlags struct {
	URL       string
	TokenFile string
	CAFile    string
}

// Connection is one resolved server connection, shared by the REST commands
// and the MCP client so both reach the same server with the same credential.
type Connection struct {
	// BaseURL is the server's address for /api/* reads.
	BaseURL string
	// MCPURL is the MCP endpoint.
	MCPURL string
	// Token is a bearer token read from a file or OBERTH_TOKEN; TokenCommand
	// is run instead when Token is empty.
	Token        string
	TokenCommand string
	// TokenFromEnvironment reports that Token came from OBERTH_TOKEN.
	TokenFromEnvironment bool
	// CACert is an inline PEM or a CA certificate file path.
	CACert string
}

// Config converts the connection to the REST client configuration.
func (connection Connection) Config() Config {
	return Config{BaseURL: connection.BaseURL, Token: connection.Token, TokenCommand: connection.TokenCommand, CACert: connection.CACert}
}

// ErrNotConfigured reports that no flag, variable or client file names a
// server; its text names every way a remote command learns one.
var ErrNotConfigured = errors.New("no Oberth server configured: pass --url https://<host>:<port> (with --token-file and --ca-file), " +
	"set OBERTH_BASE_URL (or OBERTH_MCP_URL) with OBERTH_TOKEN or OBERTH_TOKEN_COMMAND, " +
	"or keep the files oberth install writes in ~/.config/oberth (env, token, ca.crt)")

// errNoCredential names every way a remote command learns the token.
var errNoCredential = errors.New("no Oberth credential: pass --token-file <path> (mode 0600), set OBERTH_TOKEN or OBERTH_TOKEN_COMMAND, " +
	"or save the token as ~/.config/oberth/token (mode 0600)")

// ConfigDir is where oberth install writes the client files: env, ca.crt,
// mcp.json and, when the operator saves it, token. It honours
// XDG_CONFIG_HOME and otherwise uses ~/.config on macOS and Linux alike.
func ConfigDir() (string, error) {
	if base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); base != "" {
		return filepath.Join(base, "oberth"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "oberth"), nil
}

// ReadConfigEnv reads the OBERTH_* export lines of a client env file. A
// missing or unreadable file reads as empty.
func ReadConfigEnv(path string) map[string]string {
	values := map[string]string{}
	body, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own client configuration file.
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

// ConfigMCPURL reads the oberth server's url from an mcp.json client file.
func ConfigMCPURL(path string) string {
	body, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own client configuration file.
	if err != nil {
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
	return document.MCPServers["oberth"].URL
}

// ReadOwnerOnlyTokenFile reads a bearer token from a file nobody else can
// read (mode 0600 or stricter).
func ReadOwnerOnlyTokenFile(path string) (string, error) {
	info, err := os.Stat(path) //nolint:gosec // G703: the operator's own token file path.
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("token file %s has mode %04o; expected 0600 (no group/other access)", path, perm)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own token file path.
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	clear(data)
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}

// ResolveConnection is the one connection resolver of every remote command.
// Each setting takes the first of: the flag, the environment, the client
// files in ConfigDir.
//
//	address: --url; OBERTH_MCP_URL (MCP) or OBERTH_BASE_URL (REST), each
//	         deriving the other; OBERTH_BASE_URL in env; mcp.json's url
//	token:   --token-file; OBERTH_TOKEN; OBERTH_TOKEN_COMMAND;
//	         OBERTH_TOKEN_COMMAND in env; the token file
//	CA:      --ca-file; OBERTH_CA_CERT; OBERTH_CA_CERT in env; ca.crt
//
// A base address and an /mcp endpoint are both accepted wherever an address
// is: the base address is the endpoint without its /mcp path, and an
// address without a path gets /mcp for the MCP endpoint.
func ResolveConnection(flags ConnectionFlags) (Connection, error) {
	var connection Connection
	root, rootErr := ConfigDir()
	written := map[string]string{}
	configured := func(name string) string {
		if rootErr != nil {
			return ""
		}
		return filepath.Join(root, name)
	}
	if rootErr == nil {
		written = ReadConfigEnv(configured("env"))
	}
	environment := FromEnv()
	mcpEnvironment := strings.TrimSpace(os.Getenv("OBERTH_MCP_URL"))

	switch {
	case strings.TrimSpace(flags.URL) != "":
		connection.BaseURL, connection.MCPURL = addressPair(flags.URL)
	case mcpEnvironment != "" || environment.BaseURL != "":
		if mcpEnvironment != "" {
			connection.MCPURL = mcpEnvironment
		}
		if environment.BaseURL != "" {
			connection.BaseURL = strings.TrimRight(environment.BaseURL, "/")
		}
		if connection.MCPURL == "" {
			_, connection.MCPURL = addressPair(connection.BaseURL)
		}
		if connection.BaseURL == "" {
			connection.BaseURL, _ = addressPair(connection.MCPURL)
		}
	case written["OBERTH_BASE_URL"] != "" || written["OBERTH_MCP_URL"] != "":
		address := written["OBERTH_MCP_URL"]
		if written["OBERTH_BASE_URL"] != "" {
			address = written["OBERTH_BASE_URL"]
		}
		connection.BaseURL, connection.MCPURL = addressPair(address)
	default:
		if address := ConfigMCPURL(configured("mcp.json")); address != "" && rootErr == nil {
			connection.BaseURL, connection.MCPURL = addressPair(address)
		}
	}
	if connection.BaseURL == "" {
		return connection, ErrNotConfigured
	}
	if _, err := ValidateServerURL(connection.BaseURL); err != nil {
		return connection, err
	}
	if _, err := ValidateServerURL(connection.MCPURL); err != nil {
		return connection, err
	}

	for _, candidate := range []string{flags.CAFile, environment.CACert, written["OBERTH_CA_CERT"], regularFile(configured("ca.crt"))} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			connection.CACert = candidate
			break
		}
	}

	switch {
	case strings.TrimSpace(flags.TokenFile) != "":
		token, err := ReadOwnerOnlyTokenFile(flags.TokenFile)
		if err != nil {
			return connection, err
		}
		connection.Token = token
	case environment.Token != "":
		connection.Token, connection.TokenFromEnvironment = environment.Token, true
	case environment.TokenCommand != "":
		connection.TokenCommand = environment.TokenCommand
	case written["OBERTH_TOKEN_COMMAND"] != "":
		connection.TokenCommand = written["OBERTH_TOKEN_COMMAND"]
	case regularFile(configured("token")) != "":
		token, err := ReadOwnerOnlyTokenFile(configured("token"))
		if err != nil {
			return connection, err
		}
		connection.Token = token
	default:
		return connection, errNoCredential
	}
	return connection, nil
}

// addressPair derives the base address and the MCP endpoint from either.
func addressPair(address string) (base, endpoint string) {
	address = strings.TrimRight(strings.TrimSpace(address), "/")
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" {
		return address, address
	}
	if parsed.Path == "" {
		return address, address + "/mcp"
	}
	if strings.HasSuffix(parsed.Path, "/mcp") {
		return strings.TrimSuffix(address, "/mcp"), address
	}
	return address, address + "/mcp"
}

func regularFile(path string) string {
	if path == "" {
		return ""
	}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path
	}
	return ""
}
