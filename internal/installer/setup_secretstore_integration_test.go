//go:build openbao_integration

package installer

import (
	"context"
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
			rootToken, addr, cleanup := baoDevServer(t, config)
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
				args := []string{filepath.Join("../..", script), "--cli", baoPath, "--disable-transit"}
				return run(token, "bash", append(args, flags...)...)
			}
			t.Run("fresh", func(t *testing.T) {
				out, err := setup(rootToken)
				if err != nil {
					t.Fatalf("fresh setup: %v\n%s", err, out)
				}
				for _, want := range []string{"enabling Kubernetes auth at kubernetes/", "enabling KV v2 secrets mount at oberth/"} {
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
				if err != nil || strings.Contains(string(out), "» enabling ") || strings.Contains(string(out), "» writing ") {
					t.Fatalf("existing setup must be a no-op: %v\n%s", err, out)
				}
			})
			// Permit writes but deny the preceding config read: an unsuccessful
			// read must never authorize replacement, even with both force flags.
			baoWritePolicy(t, addr, rootToken, "setup-denied-read", `
path "sys/audit" { capabilities = ["read", "sudo"] }
path "sys/auth" { capabilities = ["read"] }
path "sys/mounts" { capabilities = ["read"] }
path "auth/token/lookup-self" { capabilities = ["read"] }
path "auth/kubernetes/config" { capabilities = ["update"] }
`)
			const deniedToken = "oberth-setup-denied-read-fixture"
			status, _ := baoHTTP(t, "POST", addr, "/v1/auth/token/create", rootToken,
				strings.NewReader(`{"id":"`+deniedToken+`","policies":["setup-denied-read"],"no_default_policy":true}`))
			if status != http.StatusOK {
				t.Fatalf("create restricted fixture identity: status %d", status)
			}
			for _, force := range []bool{false, true} {
				t.Run(fmt.Sprintf("existing-denied-read-force-%t", force), func(t *testing.T) {
					flags := []string{"--kubernetes-host", "https://other-cluster.example"}
					if force {
						flags = append(flags, "--force", "--force-auth-config")
					}
					out, err := setup(deniedToken, flags...)
					if err == nil || !strings.Contains(string(out), "reading auth/kubernetes/config") || !strings.Contains(string(out), "Code: 403") || strings.Contains(string(out), "writing auth/kubernetes/config") {
						t.Fatalf("denied read must stop setup before write: %v\n%s", err, out)
					}
					out, err = run(rootToken, baoPath, "read", "-field=kubernetes_host", "auth/kubernetes/config")
					if err != nil || strings.TrimSpace(string(out)) != "https://kubernetes.default.svc" {
						t.Fatalf("existing cluster config changed: %v\n%s", err, out)
					}
				})
			}
		})
	}
}
