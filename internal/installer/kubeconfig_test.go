package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user:
    token: fake-token
`

func TestK3sFallbackUnreadableK3sWithValidHomeConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	validConfig := filepath.Join(dir, "config")
	if err := os.WriteFile(validConfig, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	k3sPath := filepath.Join(dir, "k3s.yaml")
	if err := os.WriteFile(k3sPath, []byte(testKubeconfig), 0o000); err != nil {
		t.Fatal(err)
	}
	// Standard rules point at the valid config; k3s.yaml is unreadable.
	rules := &clientcmd.ClientConfigLoadingRules{
		Precedence: []string{validConfig},
	}
	var output bytes.Buffer
	_, _, selectedCtx, err := loadKubeConfigWithRules(rules, "", k3sPath, &output)
	if err != nil {
		t.Fatalf("expected success using home config, got: %v", err)
	}
	if selectedCtx != "test" {
		t.Fatalf("selected context = %q, want %q", selectedCtx, "test")
	}
	if output.Len() != 0 {
		t.Fatalf("k3s fallback message should not appear when standard config succeeds: %q", output.String())
	}
}

func TestK3sFallbackNoStandardConfigReadableK3s(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	k3sPath := filepath.Join(dir, "k3s.yaml")
	if err := os.WriteFile(k3sPath, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	// Empty precedence: no standard kubeconfig files exist.
	rules := &clientcmd.ClientConfigLoadingRules{
		Precedence: []string{filepath.Join(dir, "nonexistent")},
	}
	var output bytes.Buffer
	_, _, selectedCtx, err := loadKubeConfigWithRules(rules, "", k3sPath, &output)
	if err != nil {
		t.Fatalf("expected k3s fallback to succeed, got: %v", err)
	}
	if selectedCtx != "test" {
		t.Fatalf("selected context = %q, want %q", selectedCtx, "test")
	}
	if !strings.Contains(output.String(), "k3s kubeconfig") {
		t.Fatalf("expected k3s fallback message, got: %q", output.String())
	}
}

func TestK3sFallbackExplicitKubeconfigNeverConsultsK3s(t *testing.T) {
	// t.Setenv is required here to set KUBECONFIG, so no t.Parallel.
	dir := t.TempDir()
	validConfig := filepath.Join(dir, "config")
	if err := os.WriteFile(validConfig, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	k3sPath := filepath.Join(dir, "k3s.yaml")
	if err := os.WriteFile(k3sPath, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", validConfig)

	var output bytes.Buffer
	_, _, selectedCtx, err := loadKubeConfigForContext("", k3sPath, &output)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if selectedCtx != "test" {
		t.Fatalf("selected context = %q, want %q", selectedCtx, "test")
	}
	if output.Len() != 0 {
		t.Fatalf("k3s should not be consulted when KUBECONFIG is set: %q", output.String())
	}
}

func TestK3sFallbackExplicitKubeconfigMissingFileDoesNotFallBack(t *testing.T) {
	// t.Setenv is required here to set KUBECONFIG, so no t.Parallel.
	dir := t.TempDir()
	k3sPath := filepath.Join(dir, "k3s.yaml")
	if err := os.WriteFile(k3sPath, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	// KUBECONFIG set to a nonexistent file: the user chose this path,
	// k3s must not be consulted even though it is readable.
	t.Setenv("KUBECONFIG", filepath.Join(dir, "missing"))

	var output bytes.Buffer
	_, _, _, err := loadKubeConfigForContext("", k3sPath, &output)
	if err == nil {
		t.Fatal("expected error when KUBECONFIG points at a missing file")
	}
	if output.Len() != 0 {
		t.Fatalf("k3s should not be consulted when KUBECONFIG is explicitly set: %q", output.String())
	}
}
