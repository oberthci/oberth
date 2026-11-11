package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestClientSetupDryRunPrintsAndChangesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, key := range []string{"OBERTH_MCP_URL", "OBERTH_BASE_URL", "OBERTH_CA_CERT", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND"} {
		t.Setenv(key, "")
	}
	caFile := filepath.Join(home, "ca.crt")
	if err := os.WriteFile(caFile, []byte(testClientCA), 0o600); err != nil {
		t.Fatal(err)
	}
	var ran []string
	previous := clientSetupDeps
	clientSetupDeps = func(output io.Writer) installer.Deps {
		return installer.Deps{
			Output:   output,
			LookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
			RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
				ran = append(ran, name+" "+strings.Join(args, " "))
				return nil, errors.New("not in a dry run")
			},
		}
	}
	t.Cleanup(func() { clientSetupDeps = previous })

	var out strings.Builder
	err := runCLI(t.Context(), []string{"client", "setup", "--dry-run", "--url", "https://oberth.example:30443", "--ca-file", caFile}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, &out)
	}
	if len(ran) != 0 {
		t.Errorf("dry run ran %v", ran)
	}
	for _, want := range []string{
		"Dry run: nothing is changed.", "would be set", "NODE_EXTRA_CA_CERTS: unset -> " + filepath.Join(home, ".claude", "oberth", "ca.crt"),
		"Claude Code plugin oberth@oberthci: would be installed or configured", "claude plugin install oberth --marketplace oberthci/oberth",
		"url: https://oberth.example:30443/mcp", "ca_pin: " + testClientCAPin, "/oberth:connect", "Note: no token is saved for the plugin",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, &out)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 1 {
		t.Errorf("dry run wrote files: %v %v", entries, err)
	}
}

// `oberth client pin` prints the SHA-256 of the CA certificate and nothing
// else, from --ca-file, OBERTH_CA_CERT or the saved ca.crt, without a server.
func TestClientPinPrintsTheCAFingerprint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("OBERTH_CA_CERT", "")
	pin := func(arguments ...string) (string, error) {
		var out strings.Builder
		err := runCLI(t.Context(), append([]string{"client", "pin"}, arguments...), strings.NewReader(""), &out)
		return out.String(), err
	}
	if out, err := pin(); err == nil || !strings.Contains(err.Error(), "--ca-file") {
		t.Fatalf("pin without a CA = %q %v", out, err)
	}
	caFile := filepath.Join(home, "ca.crt")
	if err := os.WriteFile(caFile, []byte(testClientCA), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := pin("--ca-file", caFile); err != nil || out != testClientCAPin+"\n" {
		t.Fatalf("pin --ca-file = %q %v", out, err)
	}
	t.Setenv("OBERTH_CA_CERT", caFile)
	if out, err := pin(); err != nil || out != testClientCAPin+"\n" {
		t.Fatalf("pin from OBERTH_CA_CERT = %q %v", out, err)
	}
	t.Setenv("OBERTH_CA_CERT", "")
	root := filepath.Join(home, ".config", "oberth")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ca.crt"), []byte(testClientCA), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := pin(); err != nil || out != testClientCAPin+"\n" {
		t.Fatalf("pin from the saved ca.crt = %q %v", out, err)
	}
}

// A throwaway self-signed CA certificate; its key was discarded.
const testClientCA = `-----BEGIN CERTIFICATE-----
MIIBiTCCAS+gAwIBAgIUZsuYvd+8WVps2yuzKpu9WJDsaB4wCgYIKoZIzj0EAwIw
GTEXMBUGA1UEAwwOT2JlcnRoIHRlc3QgQ0EwIBcNMjYxMTEwMTIyMDMxWhgPMjEy
NjEwMTcxMjIwMzFaMBkxFzAVBgNVBAMMDk9iZXJ0aCB0ZXN0IENBMFkwEwYHKoZI
zj0CAQYIKoZIzj0DAQcDQgAEqJrzyos2qyPlWLOAJ8MJ9J9LFC9Z2brbfxZy91zb
vk4lZ4FUc+fJj1u7q7E8LBjKIdJWCr5a/sZKuGqANgGmRKNTMFEwHQYDVR0OBBYE
FH+t11dX6jkQl2fvUn1SsdR6bw6MMB8GA1UdIwQYMBaAFH+t11dX6jkQl2fvUn1S
sdR6bw6MMA8GA1UdEwEB/wQFMAMBAf8wCgYIKoZIzj0EAwIDSAAwRQIgZ75yjHqS
WYWRbwFNAoJ1SijtWfCekh76Y0RP50+tNOsCIQD/vTLvEsxSj4cZT5b6zXmvzqlo
hQYLBiw8ggdESYG5lg==
-----END CERTIFICATE-----
`

// testClientCAPin is the SHA-256 fingerprint of testClientCA.
const testClientCAPin = "7f7cf166d591b21aa64197407e41cb5cd2959d63e7ec9d5d0a8acf07f6bef75a"
