package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/client"
)

// configureClaudeMCP writes only the explicitly configured user endpoint.
// Plugin installation itself creates no MCP entry. Bearer setups use an
// owner-only file and headersHelper; certificate setups have neither.
func configureClaudeMCP(ctx context.Context, setup ClaudeCodeSetup) (ClaudeCodeChange, error) {
	change := ClaudeCodeChange{Target: "Claude Code MCP oberth", Action: "no change"}
	if setup.Token == nil && setup.ClientCert == "" && setup.ClientKey == "" {
		change.Details = []string{"Enroll a client certificate with /oberth:connect, or pass --token-file for bearer access."}
		return change, nil
	}
	if setup.DryRun {
		change.Action = "would be configured"
		return change, nil
	}
	paths, err := claudeCodePaths()
	if err != nil {
		return change, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return change, err
	}
	path := filepath.Join(home, ".claude.json")
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		path = filepath.Join(dir, ".claude.json")
	}
	document := map[string]json.RawMessage{}
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-owned Claude Code configuration path.
	if err == nil {
		if err := json.Unmarshal(data, &document); err != nil || document == nil {
			return change, errors.New("claude code configuration must be a JSON object")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return change, err
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := document["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil || servers == nil {
			return change, errors.New("claude code mcpServers must be an object")
		}
	}
	entry := map[string]any{"type": "http", "url": setup.URL, "timeout": ClaudeCodeMCPTimeoutMS}
	var previous struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(servers["oberth"], &previous)
	sameServer := client.SameServer(previous.URL, setup.URL)
	tokenPath := filepath.Join(paths.store, "bearer-token")
	if setup.Token != nil {
		if _, err := os.Stat(tokenPath); setup.ReplaceToken || !sameServer || errors.Is(err, os.ErrNotExist) {
			token, err := setup.Token(ctx)
			if err != nil {
				return change, errors.New("cannot resolve bearer credential")
			}
			if token == "" || strings.ContainsAny(token, "\r\n\"\\") {
				return change, errors.New("invalid bearer credential")
			}
			if err := os.MkdirAll(paths.store, 0700); err != nil {
				return change, err
			}
			if err := atomicWriteFile(tokenPath, []byte(token), 0600); err != nil {
				return change, err
			}
		}
		entry["headersHelper"] = headersHelperFor("cat " + shellQuoteWord(tokenPath))
	}
	if setup.ClientCert != "" || setup.ClientKey != "" {
		if err := configureClaudeCertificate(paths, setup.ClientCert, setup.ClientKey); err != nil {
			return change, err
		}
		delete(entry, "headersHelper")
	} else if setup.Token != nil {
		if err := configureClaudeCertificate(paths, "", ""); err != nil {
			return change, err
		}
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return change, err
	}
	var compact bytes.Buffer
	if len(servers["oberth"]) > 0 {
		_ = json.Compact(&compact, servers["oberth"])
	}
	if compact.String() == string(encoded) {
		return change, nil
	}
	servers["oberth"] = encoded
	document["mcpServers"], err = json.Marshal(servers)
	if err != nil {
		return change, err
	}
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return change, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil { //nolint:gosec // G703: operator-selected Claude Code configuration directory.
		return change, err
	}
	if err := atomicWriteFile(path, append(body, '\n'), 0600); err != nil {
		return change, err
	}
	change.Action = "configured"
	return change, nil
}

func configureClaudeCertificate(paths claudeCodeFiles, cert, key string) error {
	if cert != "" || key != "" {
		if _, err := client.LoadClientCertificate(cert, key); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(paths.settings)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	settings, err := parseJSONObject(data)
	if err != nil {
		return err
	}
	env, err := parseJSONObject(settings.values["env"])
	if err != nil {
		return err
	}
	if cert == "" && key == "" {
		_, hasCert := env.values["CLAUDE_CODE_CLIENT_CERT"]
		_, hasKey := env.values["CLAUDE_CODE_CLIENT_KEY"]
		if !hasCert && !hasKey {
			return nil
		}
	}
	for name, value := range map[string]string{"CLAUDE_CODE_CLIENT_CERT": cert, "CLAUDE_CODE_CLIENT_KEY": key} {
		if value == "" {
			env.remove(name)
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		env.set(name, raw)
	}
	raw, err := env.marshal()
	if err != nil {
		return err
	}
	settings.set("env", raw)
	raw, err = settings.marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(paths.settings), 0700); err != nil {
		return err
	}
	return atomicWriteFile(paths.settings, raw, 0600)
}
