package installer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Configuring an MCP client is not the same as making it able to reach the
// server. Claude Code runs on Node, and Node carries its own trust store
// rather than reading the platform's, so a private signer the operating system
// already trusts is still unknown to it: the entry this install just wrote
// fails at the TLS handshake and the client reports the server as unreachable.
//
// NODE_EXTRA_CA_CERTS is the variable Node reads for exactly this, and Claude
// Code passes the `env` object of its settings file through to the process, so
// the CA the install wrote can be named there once rather than exported by
// every shell that starts the client.

// nodeExtraCACerts is Node's own name for "trust this signer as well".
const nodeExtraCACerts = "NODE_EXTRA_CA_CERTS"

// errCATrustAlreadySet reports that the settings file already names a
// different certificate. Two deployments' CAs cannot both live in one
// variable, so the operator is told and nothing is touched: the other value is
// as likely to be the one they want as ours is.
var errCATrustAlreadySet = errors.New("already set")

// mergeNodeExtraCACerts adds the CA path to the env object of a Claude Code
// settings document, preserving everything else in the file.
//
// It returns the document to write and whether anything changed. An existing
// identical value changes nothing. An existing different value returns
// errCATrustAlreadySet, and a document that is not the shape a settings file
// has returns an error rather than a replacement: a file that will not parse
// is someone's configuration with a typo in it, and overwriting it would
// destroy every other setting they have.
func mergeNodeExtraCACerts(existing []byte, caPath string) ([]byte, bool, error) {
	document := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &document); err != nil {
			return nil, false, errors.New("not valid JSON")
		}
	}

	env := map[string]any{}
	if held, ok := document["env"]; ok {
		env, ok = held.(map[string]any)
		if !ok {
			return nil, false, errors.New(`"env" is not an object`)
		}
	}

	if held, ok := env[nodeExtraCACerts]; ok {
		if current, isString := held.(string); !isString || current != caPath {
			return nil, false, fmt.Errorf("%s %w: %v", nodeExtraCACerts, errCATrustAlreadySet, held)
		}
		return nil, false, nil
	}

	env[nodeExtraCACerts] = caPath
	document["env"] = env

	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(body, '\n'), true, nil
}

// claudeSettingsPath is where Claude Code keeps the settings it applies to
// every project, which is the scope the MCP entry was registered at.
func claudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// trustCAInClaudeCode names the CA in Claude Code's settings file and reports
// what to show in the results table.
func trustCAInClaudeCode(caPath string) (string, error) {
	path, err := claudeSettingsPath()
	if err != nil {
		return "", err
	}
	// #nosec G304 -- the path is this user's own home directory joined with a fixed name.
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", readErr
	}

	body, changed, err := mergeNodeExtraCACerts(existing, caPath)
	switch {
	case errors.Is(err, errCATrustAlreadySet):
		return "", err
	case err != nil:
		return "", fmt.Errorf("%s: %w", displayPath(path), err)
	case !changed:
		return "already trusted", nil
	}

	perm := os.FileMode(0600)
	if info, statErr := os.Stat(path); statErr == nil {
		perm = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	if err := atomicWriteFile(path, body, perm); err != nil {
		return "", err
	}
	return displayPath(path), nil
}

// caTrustOutcome is one client's answer to "can it verify this server".
type caTrustOutcome struct {
	detail string
	status string
	// note is the instruction printed under the table when the installer
	// could not arrange the trust itself. Silence here is what produces a
	// configured client that fails its first request for a reason nobody
	// named.
	note string
}

// claudeCATrust arranges the CA trust Claude Code needs, and reports what to
// show in the results table.
//
// verified says whether the CA actually completed a handshake with this
// server, so a write that could not be checked never reads as success.
//
// Claude Code is the one client here with a place in its own configuration for
// this. Anything else running on Node needs the same variable exported by the
// shell that launches it, which is what the note says.
func claudeCATrust(caPath string, verified bool) caTrustOutcome {
	detail, err := trustCAInClaudeCode(caPath)
	if err != nil {
		return caTrustOutcome{
			detail: err.Error(),
			status: "⚠ manual",
			note: fmt.Sprintf("Claude Code: set %s=%s in the env object of ~/.claude/settings.json.",
				nodeExtraCACerts, displayPath(caPath)),
		}
	}
	// A write that could not be checked is reported as exactly that. Calling
	// it success is the defect this whole step had.
	if !verified {
		return caTrustOutcome{detail: detail, status: "⚠ unverified"}
	}
	return caTrustOutcome{detail: detail, status: "✓ trusted"}
}

// verifyServerTrust performs the handshake the configured client is about to
// perform, with the same certificate pool the client was given and nothing
// else in it.
//
// A file written is not a client that connects. Every earlier version of this
// step reported success because a write returned no error, and the operator
// found out at the first request that the certificate did not verify -- wrong
// address in the certificate, a signer the Secret did not carry, a server not
// listening on the NodePort. The only evidence worth printing is a completed
// handshake, so this makes one.
func verifyServerTrust(ctx context.Context, baseURL string, caPEM []byte) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("ca.crt holds no certificate")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return err
	}
	address := parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(address, "443")
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	dialer := &tls.Dialer{Config: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}
