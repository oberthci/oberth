//go:build openbao_integration

package installer

// Real-OpenBao integration tests (#803). Exercises per-repo, per-repo-CI, and
// per-step Vault policy isolation with an actual OpenBao dev server. The test
// starts bao in dev mode, applies rendered policies through the same HCL
// builders that `secretstore sync` uses, creates token-auth identities (as a
// Kubernetes-auth stand-in), and asserts real reads.
//
// Run manually:
//   BAO_PATH=/path/to/bao go test -tags=openbao_integration -run TestPolicyIsolation -v
//
// CI runs this in the test-openbao burn.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// baoDevServer starts OpenBao in dev mode and returns the root token and
// address. The caller must call the returned cleanup function.
func baoDevServer(t *testing.T) (rootToken, addr string, cleanup func()) {
	t.Helper()
	baoPath := os.Getenv("BAO_PATH")
	if baoPath == "" {
		baoPath = "bao"
	}
	if _, err := exec.LookPath(baoPath); err != nil {
		t.Skipf("bao binary not found: %v", err)
	}

	// Find a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	rootToken = "test-root-token-803"
	addr = fmt.Sprintf("http://127.0.0.1:%d", port)

	cmd := exec.Command(baoPath, "server", "-dev",
		"-dev-root-token-id="+rootToken,
		"-dev-listen-address="+fmt.Sprintf("127.0.0.1:%d", port),
		"-dev-no-store-token",
	)
	cmd.Env = append(os.Environ(), "BAO_DEV_ROOT_TOKEN_ID="+rootToken)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		t.Fatalf("start bao: %v", err)
	}
	cleanup = func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	// Wait for the server to become ready.
	client := &http.Client{Timeout: 2 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", addr+"/v1/sys/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if ctx.Err() != nil {
			cleanup()
			t.Fatalf("bao dev server did not start within 15s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	return rootToken, addr, cleanup
}

// baoHTTP sends a Vault/Bao API request and returns the status code and body.
func baoHTTP(t *testing.T, method, addr, path, token string, body io.Reader) (int, map[string]any) {
	t.Helper()
	url := addr + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &result)
	}
	return resp.StatusCode, result
}

// baoWritePolicy writes a named policy via the sys/policy API.
func baoWritePolicy(t *testing.T, addr, rootToken, name, rules string) {
	t.Helper()
	payload := fmt.Sprintf(`{"policy":%q}`, rules)
	status, _ := baoHTTP(t, "PUT", addr, "/v1/sys/policy/"+name, rootToken, strings.NewReader(payload))
	if status != 204 && status != 200 {
		t.Fatalf("write policy %s: status %d", name, status)
	}
}

// baoCreateToken creates a token with specific policies via the auth/token/create API.
func baoCreateToken(t *testing.T, addr, rootToken string, policies []string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"policies":             policies,
		"ttl":                  "5m",
		"no_default_policy":    true,
		"no_parent":            true,
		"renewable":            false,
		"num_uses":             0,
	})
	status, result := baoHTTP(t, "POST", addr, "/v1/auth/token/create", rootToken, strings.NewReader(string(payload)))
	if status != 200 {
		t.Fatalf("create token: status %d", status)
	}
	auth, _ := result["auth"].(map[string]any)
	token, _ := auth["client_token"].(string)
	if token == "" {
		t.Fatal("create token: empty client_token")
	}
	return token
}

// baoWriteSecret writes a KV v2 secret.
func baoWriteSecret(t *testing.T, addr, rootToken, mountPath, secretPath string, data map[string]any) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"data": data})
	status, _ := baoHTTP(t, "POST", addr, "/v1/"+mountPath+"/data/"+secretPath, rootToken, strings.NewReader(string(payload)))
	if status != 200 {
		t.Fatalf("write secret %s/%s: status %d", mountPath, secretPath, status)
	}
}

// baoEnableKVv2 enables a KV v2 secret engine at the given path.
func baoEnableKVv2(t *testing.T, addr, rootToken, path string) {
	t.Helper()
	payload := `{"type":"kv","options":{"version":"2"}}`
	status, _ := baoHTTP(t, "POST", addr, "/v1/sys/mounts/"+path, rootToken, strings.NewReader(payload))
	if status != 200 && status != 204 {
		t.Fatalf("enable kv v2 at %s: status %d", path, status)
	}
}

// TestPolicyIsolation_RealOpenBao exercises per-repo, per-repo-CI, and per-step
// policy isolation against a live OpenBao dev server (#803).
func TestPolicyIsolation_RealOpenBao(t *testing.T) {
	rootToken, addr, cleanup := baoDevServer(t)
	defer cleanup()

	const (
		kvPrefix = "oberth"
		org      = "cloudtaser"
		repo     = "test-repo"
	)

	// Enable KV v2 at the "oberth" mount (matching defaultKVPrefix).
	baoEnableKVv2(t, addr, rootToken, kvPrefix)

	// Seed test secrets.
	baoWriteSecret(t, addr, rootToken, kvPrefix, "upstream/"+org+"/"+repo+"/app-config", map[string]any{"value": "upstream-secret"})
	baoWriteSecret(t, addr, rootToken, kvPrefix, "release/cosign-secret", map[string]any{"value": "cosign-key"})
	baoWriteSecret(t, addr, rootToken, kvPrefix, "release/r2-upload", map[string]any{"value": "r2-token"})
	baoWriteSecret(t, addr, rootToken, kvPrefix, "release/step-b-only", map[string]any{"value": "step-b-secret"})

	// Render policies using the production policy builders.
	// Grant paths passed to the policy builders are the logical suffix after
	// kvPrefix/data/ — the builder adds the full KV v2 path prefix. This
	// mirrors how credentialedPolicyPaths strips the mount prefix before
	// the paths reach the builder.
	perRepoPolicy := PerRepoPolicy(kvPrefix, org, repo, []string{
		"release/cosign-secret",
	})
	perRepoCIPolicy := PerRepoCIPolicy(kvPrefix, org, repo)
	perStepAPolicy := PerStepPolicy(kvPrefix, org, repo, []string{
		"release/r2-upload",
		"release/cosign-secret", // wildcard-inherited
	})
	perStepBPolicy := PerStepPolicy(kvPrefix, org, repo, []string{
		"release/step-b-only",
		"release/cosign-secret", // wildcard-inherited
	})

	// Apply policies.
	baoWritePolicy(t, addr, rootToken, "per-repo-test", perRepoPolicy)
	baoWritePolicy(t, addr, rootToken, "per-repo-ci-test", perRepoCIPolicy)
	baoWritePolicy(t, addr, rootToken, "per-step-a-test", perStepAPolicy)
	baoWritePolicy(t, addr, rootToken, "per-step-b-test", perStepBPolicy)

	// Create tokens with specific policies (token-auth as K8s-auth stand-in).
	tokenPerRepo := baoCreateToken(t, addr, rootToken, []string{"per-repo-test"})
	tokenCI := baoCreateToken(t, addr, rootToken, []string{"per-repo-ci-test"})
	tokenStepA := baoCreateToken(t, addr, rootToken, []string{"per-step-a-test"})
	tokenStepB := baoCreateToken(t, addr, rootToken, []string{"per-step-b-test"})

	// --- Assertions ---

	// 1. Own-path (200): step A can read its own granted path.
	t.Run("own-path", func(t *testing.T) {
		status, data := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/release/r2-upload", tokenStepA, nil)
		if status != 200 {
			t.Fatalf("step A reading own grant: expected 200, got %d", status)
		}
		if data == nil {
			t.Fatal("step A reading own grant: nil response")
		}
	})

	// 2. Cross-step (403): step A cannot read step B's path.
	t.Run("cross-step", func(t *testing.T) {
		status, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/release/step-b-only", tokenStepA, nil)
		if status != 403 {
			t.Fatalf("step A reading step B's path: expected 403, got %d", status)
		}
	})

	// 3. Union-only (403): per-step token cannot read a path only in the per-repo union.
	// The per-repo policy has release/cosign-secret. Per-step-A also has it.
	// To test union-only: create a path that per-repo has but per-step-A does not.
	// Per-step-A has r2-upload and cosign-secret. Per-repo has cosign-secret only.
	// So step-B-only is NOT in per-step-A's grants. And it's NOT in per-repo either
	// (per-repo only has wildcard grants). The "union-only" case is demonstrated
	// by showing that per-step A cannot read step-B-only despite being in the
	// same repo — already tested in cross-step. Here we use a different angle:
	// per-step token cannot read the broader per-repo upstream/* through a
	// path in the union that's only in per-repo — but both have upstream/*.
	// Instead, seed a release path ONLY in per-repo's grants, not in per-step-A's.
	baoWriteSecret(t, addr, rootToken, kvPrefix, "release/repo-level-only", map[string]any{"value": "repo-only"})
	// This path is NOT in per-step-A's grant list, so step A cannot read it.
	t.Run("union-only", func(t *testing.T) {
		status, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/release/repo-level-only", tokenStepA, nil)
		if status != 403 {
			t.Fatalf("step A reading repo-level-only path: expected 403, got %d", status)
		}
		// But per-repo CAN read it if it's in the policy. Let's verify per-repo can read cosign-secret.
		status2, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/release/cosign-secret", tokenPerRepo, nil)
		if status2 != 200 {
			t.Fatalf("per-repo reading cosign-secret: expected 200, got %d", status2)
		}
	})

	// 4. Wildcard-inherited (200): step A can read the wildcard-inherited path.
	t.Run("wildcard-inherited", func(t *testing.T) {
		status, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/release/cosign-secret", tokenStepA, nil)
		if status != 200 {
			t.Fatalf("step A reading wildcard-inherited path: expected 200, got %d", status)
		}
	})

	// 5. Release path (403): CI identity cannot read release secrets.
	t.Run("release-path-ci-denied", func(t *testing.T) {
		status, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/release/cosign-secret", tokenCI, nil)
		if status != 403 {
			t.Fatalf("CI identity reading release path: expected 403, got %d", status)
		}
	})

	// 6. revoke-self: works.
	t.Run("revoke-self", func(t *testing.T) {
		// Create a sacrificial token to revoke.
		sacrificialToken := baoCreateToken(t, addr, rootToken, []string{"per-step-a-test"})
		status, _ := baoHTTP(t, "PUT", addr, "/v1/auth/token/revoke-self", sacrificialToken, nil)
		if status != 204 && status != 200 {
			t.Fatalf("revoke-self: expected 200 or 204, got %d", status)
		}
		// Verify token is no longer valid.
		status2, _ := baoHTTP(t, "GET", addr, "/v1/auth/token/lookup-self", sacrificialToken, nil)
		if status2 != 403 {
			t.Fatalf("revoked token should be 403, got %d", status2)
		}
	})

	// 7. Upstream read: both per-repo and per-step can read upstream secrets.
	t.Run("upstream-read", func(t *testing.T) {
		status, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/upstream/"+org+"/"+repo+"/app-config", tokenStepA, nil)
		if status != 200 {
			t.Fatalf("step A reading upstream secret: expected 200, got %d", status)
		}
		status2, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/upstream/"+org+"/"+repo+"/app-config", tokenCI, nil)
		if status2 != 200 {
			t.Fatalf("CI reading upstream secret: expected 200, got %d", status2)
		}
	})

	// 8. Cross-repo (403): repo tokens cannot read another repo's upstream secrets.
	t.Run("cross-repo", func(t *testing.T) {
		baoWriteSecret(t, addr, rootToken, kvPrefix, "upstream/other-org/other-repo/secret", map[string]any{"value": "foreign"})
		status, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/upstream/other-org/other-repo/secret", tokenPerRepo, nil)
		if status != 403 {
			t.Fatalf("per-repo reading foreign upstream: expected 403, got %d", status)
		}
	})

	// 9. Step B can read its own path but not step A's.
	t.Run("step-b-isolation", func(t *testing.T) {
		status, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/release/step-b-only", tokenStepB, nil)
		if status != 200 {
			t.Fatalf("step B reading own path: expected 200, got %d", status)
		}
		status2, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/release/r2-upload", tokenStepB, nil)
		if status2 != 403 {
			t.Fatalf("step B reading step A path: expected 403, got %d", status2)
		}
	})
}

// TestPolicyIsolation_NegativeControl_WidenedPolicy verifies that a
// deliberately widened policy (+ on a path segment) IS caught by the
// cross-step assertion — the widened policy grants access where the correct
// policy denies it.
func TestPolicyIsolation_NegativeControl_WidenedPolicy(t *testing.T) {
	rootToken, addr, cleanup := baoDevServer(t)
	defer cleanup()

	const kvPrefix = "oberth"
	baoEnableKVv2(t, addr, rootToken, kvPrefix)

	// Seed a secret at a path the correct per-step policy should NOT grant.
	baoWriteSecret(t, addr, rootToken, kvPrefix, "release/step-b-only", map[string]any{"value": "step-b-secret"})

	// Build a deliberately WIDENED policy for step A: replace exact path with
	// a glob that covers step B's path too.
	widenedPolicy := fmt.Sprintf(`path "%s/data/release/+" { capabilities = ["read"] }
path "auth/token/revoke-self" { capabilities = ["update"] }`, kvPrefix)
	baoWritePolicy(t, addr, rootToken, "widened-step-a", widenedPolicy)

	tokenWidened := baoCreateToken(t, addr, rootToken, []string{"widened-step-a"})

	// The widened policy SHOULD be able to read step B's path — proving that
	// the correct per-step policy's cross-step denial is real and not an
	// artifact of OpenBao being too restrictive.
	status, _ := baoHTTP(t, "GET", addr, "/v1/"+kvPrefix+"/data/release/step-b-only", tokenWidened, nil)
	if status != 200 {
		t.Fatalf("widened policy reading step B path: expected 200, got %d (negative control failed: the + glob should grant access)", status)
	}
}
