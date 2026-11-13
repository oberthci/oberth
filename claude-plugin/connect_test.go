package plugin_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
)

func pluginWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestConnectConfiguresOnlyVerifiedMCPEndpoint(t *testing.T) {
	for _, endpoint := range []string{"https://server.test", "https://server.test/", "https://server.test/mcp", "https://server.test/prefix", "https://server.test/prefix/", "https://server.test/prefix/mcp/", "https://server.test/prefix%2Fmcp", "https://server.test/prefix/%6dcp", "https://server.test/wrong"} {
		t.Run(endpoint, func(t *testing.T) {
			root, bin := t.TempDir(), t.TempDir()
			store := filepath.Join(root, "oberth")
			if err := os.Mkdir(store, 0700); err != nil {
				t.Fatal(err)
			}
			pluginWrite(t, filepath.Join(store, "ca.crt"), "public CA", 0600)
			settings := `{"env":{"CLAUDE_CODE_CLIENT_CERT":"/local/cert","CLAUDE_CODE_CLIENT_KEY":"/local/key"},"model":"keep"}`
			pluginWrite(t, filepath.Join(root, "settings.json"), settings, 0600)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			pluginWrite(t, filepath.Join(bin, "oberth"), fmt.Sprintf("#!/bin/sh\nexec %s -test.run='^TestConnectCLIProbeHelper$' -- %s \"$@\"\n", pluginShellWord(binary), pluginShellWord(root)), 0700)
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
				probed, probeErr := os.ReadFile(filepath.Join(root, "probed-url"))
				expected, resolveErr := client.ResolveConnection(client.ConnectionFlags{URL: endpoint, CAFile: filepath.Join(store, "ca.crt"), ClientCert: "/local/cert", ClientKey: "/local/key"})
				if resolveErr != nil || string(probed) != expected.MCPURL {
					t.Fatalf("normalization differs from CLI: %s vs %s: %v", probed, expected.MCPURL, resolveErr)
				}
				if err != nil || probeErr != nil || string(raw) != string(probed)+"\n" {
					t.Fatalf("endpoint: %s %v", raw, err)
				}
				registration, err := os.ReadFile(filepath.Join(root, "registration"))
				if err != nil {
					t.Fatal(err)
				}
				var entry struct {
					URL string `json:"url"`
				}
				lines := strings.Split(strings.TrimSpace(string(registration)), "\n")
				if json.Unmarshal([]byte(lines[len(lines)-1]), &entry) != nil || entry.URL != string(probed) {
					t.Fatalf("registered endpoint differs from CLI probe: %s vs %s", registration, probed)
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

func pluginShellWord(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// The probe stub uses the real CLI connection resolver, so rewriting by the
// CLI cannot hide a different URL stored in the registration.
func TestConnectCLIProbeHelper(t *testing.T) {
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		return
	}
	if len(args) < 3 || args[1] != "mcp" || args[2] != "tools" {
		os.Exit(91)
	}
	var cfg client.ConnectionFlags
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.URL, "url", "", "")
	flags.StringVar(&cfg.CAFile, "ca-file", "", "")
	flags.StringVar(&cfg.ClientCert, "client-cert", "", "")
	flags.StringVar(&cfg.ClientKey, "client-key", "", "")
	flags.StringVar(&cfg.TokenFile, "token-file", "", "")
	flags.String("timeout", "", "")
	flags.String("reconnect-window", "", "")
	if flags.Parse(args[3:]) != nil {
		os.Exit(92)
	}
	resolved, err := client.ResolveConnection(cfg)
	if err != nil {
		os.Exit(93)
	}
	if os.WriteFile(filepath.Join(args[0], "probe"), []byte(strings.Join(args[1:], "\n")), 0600) != nil {
		os.Exit(94)
	}
	if os.WriteFile(filepath.Join(args[0], "probed-url"), []byte(resolved.MCPURL), 0600) != nil {
		os.Exit(94)
	}
	endpoint, err := url.Parse(resolved.MCPURL)
	if err != nil || (endpoint.Path != "/mcp" && endpoint.Path != "/prefix/mcp") {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestConnectRejectsTokenPathBeforeProbe(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	marker := filepath.Join(root, "probe")
	pluginWrite(t, filepath.Join(bin, "oberth"), "#!/bin/sh\ntouch "+pluginShellWord(marker)+"\nexit 1\n", 0700)
	pluginWrite(t, filepath.Join(bin, "claude"), "#!/bin/sh\nexit 90\n", 0700)
	cmd := exec.Command("sh", "scripts/trust", "configure", "https://server.test", filepath.Join(root, "invalid token path"))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "CLAUDE_CONFIG_DIR="+root)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "token file path must contain") {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("invalid token path reached probe")
	}
}
