package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Model tool-owned credential refresh through the actual plugin -> CLI ->
// installer runner boundary, before the test binary's environment reset.
func init() {

	var err error
	switch filepath.Base(os.Args[0]) {
	case "oberth":
		if len(os.Args) < 3 || os.Args[1] != "client" {
			err = fmt.Errorf("unexpected proxy command")
		} else {
			err = runClient(context.Background(), os.Args[2:], os.Stdout)
		}
	case "kubectl":
		err = refreshProxyCredential()
	default:
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
func refreshProxyCredential() error {
	path := os.Getenv("KUBECONFIG")
	if len(os.Args) < 3 || os.Args[1] != "--kubeconfig" || os.Args[2] != path {
		return fmt.Errorf("private target differs between arguments and environment")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return err
	}
	if cfg.AuthConfigPersister == nil {
		return fmt.Errorf("refresh fixture missing persister")
	}
	return cfg.AuthConfigPersister.Persist(map[string]string{"id-token": "refreshed-fixture"})
}
func TestAddRepoKubectlUsesPrivateConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	first, second := clientcmdapi.NewConfig(), clientcmdapi.NewConfig()
	first.Clusters["cluster"] = &clientcmdapi.Cluster{Server: "https://api.example.test"}
	first.Contexts["chosen"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "operator"}
	first.CurrentContext = "chosen"
	second.AuthInfos["operator"] = &clientcmdapi.AuthInfo{AuthProvider: &clientcmdapi.AuthProviderConfig{Name: "oidc", Config: map[string]string{"id-token": "original-fixture"}}}
	paths := []string{filepath.Join(root, "contexts"), filepath.Join(root, "credentials")}
	original := map[string][]byte{}
	for i, cfg := range []*clientcmdapi.Config{first, second} {
		if err := clientcmd.WriteToFile(*cfg, paths[i]); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		original[paths[i]] = data
	}
	for _, name := range []string{"oberth", "kubectl"} {
		if err := os.Symlink(os.Args[0], filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KUBECONFIG", strings.Join(paths, string(os.PathListSeparator)))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := filepath.Join("..", "..", "claude-plugin", "scripts", "add-repo")
	for _, args := range [][]string{{"contexts"}, {"pods", "chosen", "pipelines"}, {"prepare-upstream", "chosen", "pipelines", "pod", "forge", "ssh://code.example.test/org", "repo", "shared", "--consent"}, {"verify", "chosen", "pipelines", "pod", "forge/org/repo"}} {
		cmd := exec.CommandContext(t.Context(), "sh", append([]string{script}, args...)...)
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("plugin proxy: %v %s", err, data)
		}
		for path, before := range original {
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("plugin changed source kubeconfig", path, err)
			}
			if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
				t.Fatal("plugin left source lock", path, err)
			}
		}
	}
}
