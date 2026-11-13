package plugin_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func pluginWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestConnectConfiguresOnlyVerifiedMCPEndpoint(t *testing.T) {
	for _, endpoint := range []string{"https://server.test", "https://server.test/", "https://server.test/mcp", "https://server.test/wrong"} {
		t.Run(endpoint, func(t *testing.T) {
			root, bin := t.TempDir(), t.TempDir()
			store := filepath.Join(root, "oberth")
			if err := os.Mkdir(store, 0700); err != nil {
				t.Fatal(err)
			}
			pluginWrite(t, filepath.Join(store, "ca.crt"), "public CA", 0600)
			settings := `{"env":{"CLAUDE_CODE_CLIENT_CERT":"/local/cert","CLAUDE_CODE_CLIENT_KEY":"/local/key"},"model":"keep"}`
			pluginWrite(t, filepath.Join(root, "settings.json"), settings, 0600)
			pluginWrite(t, filepath.Join(bin, "oberth"), `#!/bin/sh
printf '%s\n' "$@" >"$CLAUDE_CONFIG_DIR/probe"
[ "$1 $2" = 'mcp tools' ] || exit 91
case "$4" in https://server.test/mcp) exit 0;; *) exit 1;; esac
`, 0700)
			pluginWrite(t, filepath.Join(bin, "claude"), `#!/bin/sh
printf '%s\n' "$@" >>"$CLAUDE_CONFIG_DIR/registration"
`, 0700)
			cmd := exec.Command("sh", "scripts/trust", "configure", endpoint)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "CLAUDE_CONFIG_DIR="+root)
			out, err := cmd.CombinedOutput()
			bad := strings.HasSuffix(endpoint, "wrong")
			if bad {
				if err == nil || !strings.Contains(string(out), "did not answer as authenticated MCP") {
					t.Fatalf("%v %s", err, out)
				}
				if _, err := os.Stat(filepath.Join(root, "registration")); !os.IsNotExist(err) {
					t.Fatal("failed probe modified registration")
				}
			} else {
				if err != nil {
					t.Fatalf("%v %s", err, out)
				}
				raw, err := os.ReadFile(filepath.Join(store, "url"))
				if err != nil || string(raw) != "https://server.test/mcp\n" {
					t.Fatalf("endpoint: %s %v", raw, err)
				}
			}
			raw, err := os.ReadFile(filepath.Join(root, "settings.json"))
			if err != nil || string(raw) != settings {
				t.Fatal("certificate settings changed")
			}
			probe, err := os.ReadFile(filepath.Join(root, "probe"))
			if err != nil || !strings.Contains(string(probe), "--ca-file\n"+store+"/ca.crt") || !strings.Contains(string(probe), "--client-cert\n/local/cert") {
				t.Fatalf("wrong TLS probe: %s %v", probe, err)
			}
		})
	}
}

func TestConnectBearerProbeUsesFileAndPreservesSettingsOnFailure(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	pluginWrite(t, filepath.Join(root, "oberth", "ca.crt"), "CA", 0600)
	pluginWrite(t, filepath.Join(root, "settings.json"), `{"env":{"CLAUDE_CODE_CLIENT_CERT":"keep"}}`, 0600)
	token := filepath.Join(root, "token")
	pluginWrite(t, token, "never print this credential", 0600)
	pluginWrite(t, filepath.Join(bin, "oberth"), "#!/bin/sh\nprintf '%s\\n' \"$@\" >\"$CLAUDE_CONFIG_DIR/probe\"\nexit 1\n", 0700)
	pluginWrite(t, filepath.Join(bin, "claude"), "#!/bin/sh\nexit 90\n", 0700)
	cmd := exec.Command("sh", "scripts/trust", "configure", "https://server.test", token)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "CLAUDE_CONFIG_DIR="+root)
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "never print") {
		t.Fatalf("%v %s", err, out)
	}
	probe, _ := os.ReadFile(filepath.Join(root, "probe"))
	if !strings.Contains(string(probe), "--token-file\n"+token) || strings.Contains(string(probe), "never print") {
		t.Fatal("probe did not pass token file safely")
	}
	var settings map[string]any
	raw, _ := os.ReadFile(filepath.Join(root, "settings.json"))
	if json.Unmarshal(raw, &settings) != nil || settings["env"].(map[string]any)["CLAUDE_CODE_CLIENT_CERT"] != "keep" {
		t.Fatal("failed probe cleared credentials")
	}
}
