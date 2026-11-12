package kubeconfig

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestPrivateMergePreservesPrecedenceContextAndRelativePaths(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	firstDir, secondDir := filepath.Join(root, "first"), filepath.Join(root, "second")
	for _, directory := range []string{firstDir, secondDir} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	first, second := clientcmdapi.NewConfig(), clientcmdapi.NewConfig()
	first.Clusters["cluster"] = &clientcmdapi.Cluster{Server: "https://chosen.example.test", CertificateAuthority: "ca.pem"}
	first.Contexts["selected"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "operator"}
	first.CurrentContext = "selected"
	second.Clusters["cluster"] = &clientcmdapi.Cluster{Server: "https://ignored.example.test"}
	second.Contexts["other"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "operator"}
	second.CurrentContext = "other"
	second.AuthInfos["operator"] = &clientcmdapi.AuthInfo{ClientCertificate: "cert.pem", ClientKey: "key.pem", TokenFile: "token", Exec: &clientcmdapi.ExecConfig{Command: "./credential", APIVersion: "client.authentication.k8s.io/v1", InteractiveMode: clientcmdapi.NeverExecInteractiveMode}}
	firstPath, secondPath := filepath.Join(firstDir, "config"), filepath.Join(secondDir, "config")
	for path, cfg := range map[string]*clientcmdapi.Config{firstPath: first, secondPath: second} {
		if err := clientcmd.WriteToFile(*cfg, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KUBECONFIG", firstPath+string(os.PathListSeparator)+secondPath)
	remote := []string{"exec", "pod", "--", "--kubeconfig", "remote-file"}
	args, environment, _, cleanup, err := Command("kubectl", append([]string{"--context=other"}, remote...), []string{"HOME=/operator", "HELM_DATA_HOME=/retained-helm"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !slices.Equal(args[4:], remote) {
		t.Fatal("remote arguments changed", args)
	}
	if args[0] != "--kubeconfig" || args[2] != "--context" || args[3] != "other" {
		t.Fatal(args)
	}
	private := args[1]
	loaded, err := clientcmd.LoadFromFile(private)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CurrentContext != "other" || loaded.Clusters["cluster"].Server != "https://chosen.example.test" {
		t.Fatal("precedence/context lost", loaded.CurrentContext)
	}
	auth := loaded.AuthInfos["operator"]
	for got, want := range map[string]string{loaded.Clusters["cluster"].CertificateAuthority: filepath.Join(firstDir, "ca.pem"), auth.ClientCertificate: filepath.Join(secondDir, "cert.pem"), auth.ClientKey: filepath.Join(secondDir, "key.pem"), auth.TokenFile: filepath.Join(secondDir, "token"), auth.Exec.Command: filepath.Join(secondDir, "credential")} {
		if got != want {
			t.Fatalf("source-relative path=%q want=%q", got, want)
		}
	}
	if !slices.Contains(environment, "KUBECONFIG="+private) || !slices.Contains(environment, "HOME="+filepath.Dir(private)) || !slices.Contains(environment, "HELM_DATA_HOME=/retained-helm") {
		t.Fatal("private environment lost", environment)
	}
	for _, entry := range auth.Exec.Env {
		if entry.Name == "HOME" && entry.Value == "" {
			t.Fatal("exec HOME lost")
		}
	}
	cleanup()
	if _, err := os.Stat(private); !os.IsNotExist(err) {
		t.Fatal("private config remains", err)
	}
}

func TestNativeClientCannotPersistOrMigrate(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "missing-default")
	raw := clientcmdapi.NewConfig()
	raw.Clusters["cluster"] = &clientcmdapi.Cluster{Server: "https://api.example.test"}
	raw.Contexts["context"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "operator"}
	raw.CurrentContext = "context"
	raw.AuthInfos["operator"] = &clientcmdapi.AuthInfo{AuthProvider: &clientcmdapi.AuthProviderConfig{Name: "oidc", Config: map[string]string{"id-token": "fixture"}}}
	if err := clientcmd.WriteToFile(*raw, source); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	rules := Rules(source)
	rules.MigrationRules = map[string]string{destination: source}
	_, config, err := Client(rules, "context")
	if err != nil || config.AuthConfigPersister != nil {
		t.Fatal("native persistence enabled", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("read migrated a config", err)
	}
	after, err := os.ReadFile(source)
	if err != nil || string(before) != string(after) {
		t.Fatal("source changed", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "source" {
		t.Fatal("source siblings changed", entries, err)
	}
}

func TestPrivateArgumentsRespectSeparatorAndLastExplicitTarget(t *testing.T) {
	args := []string{"--kubeconfig=first", "--context=ignored", "--kubeconfig", "last", "--kube-context", "chosen", "exec", "pod", "--", "--kubeconfig", "remote"}
	path, context, forward, err := commandTarget(args)
	if err != nil || path != "last" || context != "chosen" || !strings.HasSuffix(strings.Join(forward, " "), "exec pod -- --kubeconfig remote") {
		t.Fatal(path, context, forward, err)
	}
	if _, _, _, err := commandTarget([]string{"--kubeconfig"}); err == nil {
		t.Fatal("missing flag value accepted")
	}
}
