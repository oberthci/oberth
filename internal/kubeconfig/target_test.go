package kubeconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitContextLeavesInputUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	raw := []byte("apiVersion: v1\nkind: Config\ncurrent-context: other\nclusters:\n- name: selected\n  cluster:\n    server: https://127.0.0.1:6443\ncontexts:\n- name: selected\n  context:\n    cluster: selected\n- name: other\n  context:\n    cluster: absent\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	target, err := Open(path, "selected")
	if err != nil || target.Config.Host != "https://127.0.0.1:6443" {
		t.Fatal(target, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(raw) {
		t.Fatal("kubeconfig changed", err)
	}
	if _, err = Open(path, ""); err == nil {
		t.Fatal("implicit context accepted")
	}
	if _, err = Open(path, "missing"); err == nil {
		t.Fatal("missing context accepted")
	}
}

func TestCredentialPluginContextAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	raw := `apiVersion: v1
kind: Config
clusters:
- name: selected
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: selected
  context:
    cluster: selected
    user: login
users:
- name: login
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: oberth-login
      interactiveMode: Never
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	target, err := Open(path, "selected")
	if err != nil || target.Config.ExecProvider == nil || target.Config.ExecProvider.Command != "oberth-login" {
		t.Fatal(target, err)
	}
}
