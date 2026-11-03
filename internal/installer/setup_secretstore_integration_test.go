//go:build openbao_integration

package installer

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run with BAO_PATH=/path/to/bao go test -tags=openbao_integration
// ./internal/installer -run '^TestSetupSecretStoreOpenBao$'. All store state,
// audit files and fixed test credentials belong to disposable loopback servers.
func TestSetupSecretStoreOpenBao(t *testing.T) {
	baoPath := os.Getenv("BAO_PATH")
	if baoPath == "" {
		baoPath = "bao"
	}
	if _, err := exec.LookPath(baoPath); err != nil {
		t.Skipf("bao binary not found: %v", err)
	}
	for _, script := range []string{"scripts/setup-secretstore.sh", "website/public/setup-secretstore.sh"} {
		t.Run(script, func(t *testing.T) {
			dir := t.TempDir()
			config := filepath.Join(dir, "server.hcl")
			if err := os.WriteFile(config, []byte(fmt.Sprintf("audit \"file\" \"oberth\" {\n  options { file_path = %q }\n}\n", filepath.Join(dir, "audit.log"))), 0600); err != nil {
				t.Fatal(err)
			}
			rootToken, addr, baoCA, cleanup := baoDevServerWithTLS(t, true, config)
			defer cleanup()
			// Use only the public certificate from a disposable TLS fixture as the
			// cluster CA. No Kubernetes access or kubeconfig is needed.
			tlsFixture := httptest.NewTLSServer(http.NotFoundHandler())
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsFixture.Certificate().Raw})
			tlsFixture.Close()
			caPath := filepath.Join(dir, "cluster-ca.pem")
			if err := os.WriteFile(caPath, ca, 0600); err != nil {
				t.Fatal(err)
			}
			env := []string{}
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "BAO_") && !strings.HasPrefix(entry, "VAULT_") && !strings.HasPrefix(entry, "KUBERNETES_") && !strings.HasPrefix(entry, "IN_CLUSTER_CA=") {
					env = append(env, entry)
				}
			}
			env = append(env, "BAO_ADDR="+addr, "VAULT_ADDR="+addr,
				"BAO_CACERT="+baoCA, "VAULT_CACERT="+baoCA,
				"KUBERNETES_SERVICE_HOST=127.0.0.1", "IN_CLUSTER_CA="+caPath)
			run := func(token, program string, args ...string) ([]byte, error) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, program, args...)
				cmd.Env = append(append([]string{}, env...), "BAO_TOKEN="+token, "VAULT_TOKEN="+token)
				return cmd.CombinedOutput()
			}
			// Pin the real CLI diagnostics that the old mock misrepresented.
			for path, diagnostic := range map[string]string{
				"sys/auth/kubernetes":       "* No auth engine at kubernetes/",
				"sys/mounts/oberth":         "* No secret engine mount at oberth/",
				"sys/mounts/oberth-transit": "* No secret engine mount at oberth-transit/",
			} {
				out, err := run(rootToken, baoPath, "read", "-field=type", path)
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), "Code: 400") || !strings.Contains(string(out), diagnostic) {
					t.Fatalf("missing mount %s: err=%v output=%s", path, err, out)
				}
			}
			setup := func(token string, flags ...string) ([]byte, error) {
				args := []string{filepath.Join("../..", script), "--cli", baoPath}
				return run(token, "bash", append(args, flags...)...)
			}
			t.Run("fresh", func(t *testing.T) {
				out, err := setup(rootToken)
				if err != nil {
					t.Fatalf("fresh setup: %v\n%s", err, out)
				}
				for _, want := range []string{"enabling Kubernetes auth at kubernetes/", "enabling KV v2 secrets mount at oberth/", "enabling Transit mount at oberth-transit/", "creating non-exportable Transit key trusted-plan-artifacts"} {
					if !strings.Contains(string(out), want) {
						t.Errorf("fresh setup omitted %q: %s", want, out)
					}
				}
			})
			// Continue only when the real fresh-store mutations succeeded.
			if t.Failed() {
				return
			}
			t.Run("existing", func(t *testing.T) {
				out, err := setup(rootToken)
				if err != nil || strings.Contains(string(out), "» enabling ") || strings.Contains(string(out), "» writing ") || strings.Contains(string(out), "» creating ") {
					t.Fatalf("existing setup must be a no-op: %v\n%s", err, out)
				}
			})
			// Exercise the actual key, including encryption/decryption over verified TLS.
			t.Run("transit", func(t *testing.T) {
				out, err := run(rootToken, baoPath, "read", "-format=json", "oberth-transit/keys/trusted-plan-artifacts")
				var key struct {
					Data map[string]any `json:"data"`
				}
				if err != nil || json.Unmarshal(out, &key) != nil {
					t.Fatalf("read Transit key: %v", err)
				}
				for field, want := range map[string]any{
					"type": "aes256-gcm96", "derived": false, "exportable": false,
					"allow_plaintext_backup": false, "supports_encryption": true, "supports_decryption": true,
				} {
					if key.Data[field] != want {
						t.Errorf("Transit key %s = %v, want %v", field, key.Data[field], want)
					}
				}
				const plaintext = "b2JlcnRoLWZpeHR1cmU="
				out, err = run(rootToken, baoPath, "write", "-field=ciphertext", "oberth-transit/encrypt/trusted-plan-artifacts", "plaintext="+plaintext)
				if err != nil {
					t.Fatalf("Transit encrypt: %v", err)
				}
				out, err = run(rootToken, baoPath, "write", "-field=plaintext", "oberth-transit/decrypt/trusted-plan-artifacts", "ciphertext="+strings.TrimSpace(string(out)))
				if err != nil || strings.TrimSpace(string(out)) != plaintext {
					t.Fatalf("Transit round trip failed: %v", err)
				}
			})
			// Compare the stored data, excluding changing response metadata.
			snapshot := func(path string) string {
				t.Helper()
				out, err := run(rootToken, baoPath, "read", "-format=json", path)
				var response struct {
					Data map[string]any `json:"data"`
				}
				if err != nil || json.Unmarshal(out, &response) != nil || response.Data == nil {
					t.Fatalf("snapshot %s failed: %v", path, err)
				}
				data, err := json.Marshal(response.Data)
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			// Permit updates but deny the preceding read. Neither force flag may
			// turn a failed auth-config or policy read into a replacement write.
			for _, denied := range []struct{ name, path, diagnostic string }{
				{"auth-config", "auth/kubernetes/config", "reading auth/kubernetes/config"},
				{"policy", "sys/policies/acl/oberth-ci", "reading policy oberth-ci"},
			} {
				policy := `
path "sys/audit" { capabilities = ["read", "sudo"] }
path "sys/auth" { capabilities = ["read"] }
path "sys/mounts" { capabilities = ["read"] }
path "auth/token/lookup-self" { capabilities = ["read"] }
path "auth/kubernetes/config" { capabilities = ["read", "update"] }
path "sys/mounts/oberth" { capabilities = ["read"] }
path "oberth-transit/keys/trusted-plan-artifacts" { capabilities = ["read"] }
path "sys/policies/acl/oberth-ci" { capabilities = ["read", "update"] }
path "auth/kubernetes/role/oberth-ci" { capabilities = ["read", "update"] }
`
				policy = strings.Replace(policy,
					fmt.Sprintf("path %q { capabilities = [\"read\", \"update\"] }", denied.path),
					fmt.Sprintf("path %q { capabilities = [\"update\"] }", denied.path), 1)
				policyPath := filepath.Join(dir, "denied-read.hcl")
				if err := os.WriteFile(policyPath, []byte(policy), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := run(rootToken, baoPath, "policy", "write", "setup-denied-read", policyPath); err != nil {
					t.Fatalf("write fixture policy: %v", err)
				}
				deniedToken := "oberth-setup-denied-" + denied.name + "-fixture"
				if _, err := run(rootToken, baoPath, "token", "create", "-id="+deniedToken, "-policy=setup-denied-read", "-no-default-policy"); err != nil {
					t.Fatalf("create restricted fixture identity: %v", err)
				}
				for _, force := range []bool{false, true} {
					t.Run(fmt.Sprintf("existing-denied-%s-read-force-%t", denied.name, force), func(t *testing.T) {
						paths := []string{"sys/policies/acl/oberth-ci", "auth/kubernetes/role/oberth-ci", "auth/kubernetes/config"}
						before := make(map[string]string)
						for _, path := range paths {
							before[path] = snapshot(path)
						}
						var flags []string
						if denied.name == "auth-config" {
							flags = append(flags, "--kubernetes-host", "https://other-cluster.example")
						}
						if force {
							flags = append(flags, "--force", "--force-auth-config")
						}
						out, err := setup(deniedToken, flags...)
						if err == nil || !strings.Contains(string(out), denied.diagnostic) || !strings.Contains(string(out), "Code: 403") {
							t.Fatalf("denied read must stop setup: %v\n%s", err, out)
						}
						for _, path := range paths {
							if snapshot(path) != before[path] {
								t.Errorf("failed read changed %s", path)
							}
						}
					})
				}
			}
		})
	}
}
