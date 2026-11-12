package installer

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/invite"
)

func TestPluginInviteKeepsKeysAndCredentialLocal(t *testing.T) {
	requireOpenSSL(t)
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Fatal("ssh-keygen is required for invitation tests")
	}
	for _, shell := range trustShells() {
		t.Run(shell, func(t *testing.T) {
			root := t.TempDir()
			claude := filepath.Join(root, "claude")
			tools := filepath.Join(root, "bin")
			if err := os.Mkdir(tools, 0700); err != nil {
				t.Fatal(err)
			}
			secret, _, err := invite.NewSecret()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "invite.json")
			if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
				t.Fatal(err)
			}
			stub := `#!/bin/sh
set -eu
[ "$1" = client ] && [ "$2" = redeem ]
shift 2
while [ "$#" -gt 0 ]; do
 case "$1" in
 --csr) cp "$2" "$INVITE_TEST_ROOT/request.csr";;
 --ssh-public-key) cp "$2" "$INVITE_TEST_ROOT/request.pub";;
 --invite-file) [ "$2" = "$INVITE_TEST_ROOT/invite.json" ];;
 --certificate-out) :;;
 *) exit 90;;
 esac
 shift 2
done
exit 1
`
			if err := os.WriteFile(filepath.Join(tools, "oberth"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			environment := []string{"INVITE_TEST_ROOT=" + root}
			for _, variable := range os.Environ() {
				if value, ok := strings.CutPrefix(variable, "PATH="); ok {
					environment = append(environment, "PATH="+tools+":"+value)
				}
			}
			out, err := runTrust(t, shell, claude, environment, "redeem", path)
			if err == nil || strings.Contains(out, secret) || strings.Contains(out, "PRIVATE KEY") {
				t.Fatal("invitation script leaked a secret or ignored refusal")
			}
			if _, err := os.Stat(filepath.Join(root, "request.csr")); err != nil {
				t.Fatalf("script stopped before enrollment: %s", out)
			}
			for _, key := range []string{"current/client.key", "push_ed25519"} {
				info, err := os.Stat(filepath.Join(claude, "oberth", key))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("private local key missing or unsafe", err)
				}
			}
			if !strings.Contains(string(mustRead(t, filepath.Join(root, "request.csr"))), "BEGIN CERTIFICATE REQUEST") || !strings.HasPrefix(string(mustRead(t, filepath.Join(root, "request.pub"))), "ssh-ed25519 ") {
				t.Fatal("enrollment did not receive the public CSR and SSH key")
			}
		})
	}
}

func TestPluginInviteConfiguresMutualTLSAfterRedemption(t *testing.T) {
	requireOpenSSL(t)
	authority := newTestAuthorityFor(t, "Invite test CA", "localhost")
	leaf, key := authority.issue(t)
	address := servePresenting(t, key, leaf, authority.certificate.Raw)
	for _, shell := range trustShells() {
		t.Run(shell, func(t *testing.T) {
			root := t.TempDir()
			claude := filepath.Join(root, "claude")
			tools := filepath.Join(root, "bin")
			if err := os.MkdirAll(tools, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(claude, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(claude, "settings.json"), []byte(`{"env":{"KEEP":"value"},"model":"chosen"}`), 0600); err != nil {
				t.Fatal(err)
			}
			raw, err := x509.MarshalECPrivateKey(authority.key)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "ca.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: raw}), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "ca.crt"), authority.pem, 0600); err != nil {
				t.Fatal(err)
			}
			stub := `#!/bin/sh
set -eu
case "$1 $2" in
 'client redeem')
 shift 2
 while [ "$#" -gt 0 ]; do
  case "$1" in --csr) csr=$2;; --certificate-out) cert=$2;; --invite-file|--ssh-public-key) :;; *) exit 90;; esac
  shift 2
 done
 openssl x509 -req -in "$csr" -CA "$INVITE_TEST_ROOT/ca.crt" -CAkey "$INVITE_TEST_ROOT/ca.key" -CAcreateserial -days 1 -out "$cert" >/dev/null 2>&1
 cp "$INVITE_TEST_ROOT/ca.crt" "$cert.ca.crt"
 printf '%s\n' "$INVITE_TEST_URL" >"$cert.url"
 ;;
 'client pin') openssl x509 -in "$4" -noout -fingerprint -sha256 | sed 's/^.*=//' | tr -d ':\r\n' | tr A-F a-f;;
 *) exit 90;;
esac
`
			if err := os.WriteFile(filepath.Join(tools, "oberth"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			claudeStub := `#!/bin/sh
set -eu
if [ "$2" = add-json ]; then printf '%s\n' "$6" >"$INVITE_TEST_ROOT/mcp.json"; fi
`
			if err := os.WriteFile(filepath.Join(tools, "claude"), []byte(claudeStub), 0700); err != nil {
				t.Fatal(err)
			}
			environment := []string{"INVITE_TEST_ROOT=" + root, "INVITE_TEST_URL=" + address}
			for _, variable := range os.Environ() {
				if value, ok := strings.CutPrefix(variable, "PATH="); ok {
					environment = append(environment, "PATH="+tools+":"+value)
				}
			}
			out, err := runTrust(t, shell, claude, environment, "redeem", filepath.Join(root, "invite.json"))
			if err != nil {
				t.Fatalf("invite setup: %v %s", err, out)
			}
			if strings.Contains(out, "PRIVATE KEY") {
				t.Fatal("private material appeared in output")
			}
			var settings struct {
				Env   map[string]string `json:"env"`
				Model string            `json:"model"`
			}
			if err := json.Unmarshal(mustRead(t, filepath.Join(claude, "settings.json")), &settings); err != nil {
				t.Fatal(err)
			}
			if settings.Env["KEEP"] != "value" || settings.Model != "chosen" || settings.Env["NODE_EXTRA_CA_CERTS"] == "" || settings.Env["CLAUDE_CODE_CLIENT_CERT"] == "" || settings.Env["CLAUDE_CODE_CLIENT_KEY"] == "" {
				t.Fatal("invite setup lost settings or client identity")
			}
			var endpoint map[string]any
			if err := json.Unmarshal(mustRead(t, filepath.Join(root, "mcp.json")), &endpoint); err != nil {
				t.Fatal(err)
			}
			if endpoint["url"] != address || endpoint["headers"] != nil || endpoint["headersHelper"] != nil {
				t.Fatal("invite setup did not configure certificate-only MCP")
			}
		})
	}
}
