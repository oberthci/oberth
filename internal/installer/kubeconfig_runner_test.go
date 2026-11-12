package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestReadOnlyLoaderDoesNotMigrateConfiguration(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		root := t.TempDir()
		source := filepath.Join(root, "migration-source")
		dest := filepath.Join(root, "default")
		selected := filepath.Join(root, "selected")
		for _, p := range []string{source, selected} {
			if err := os.WriteFile(p, []byte(testKubeconfig), 0600); err != nil {
				t.Fatal(err)
			}
		}
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		// Redirect every migration destination to this fixture; never inspect operator config.
		rules.MigrationRules = map[string]string{dest: source}
		rules.Precedence = []string{dest}
		if explicit {
			rules.ExplicitPath = selected
		}
		if _, _, _, err := loadFromRules(rules, ""); explicit && err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(dest)
		if !os.IsNotExist(err) {
			t.Fatalf("read created migration destination: %v", err)
		}
		t.Logf("explicit=%v: read left the default destination absent", explicit)
	}
}

func init() {
	name := filepath.Base(os.Args[0])
	if (name != "kubectl" && name != "helm") || len(os.Args) < 3 || (os.Args[len(os.Args)-2] != "fixture-persist" && os.Args[len(os.Args)-2] != "fixture-cancel") {
		return
	}
	// Run before repository TestMain clears KUBECONFIG. Model an external client-go tool.
	if err := reviewPersistChild(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
func reviewPersistChild() error {
	path := os.Getenv("KUBECONFIG")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		return errors.New("private configuration is not an owner-only file")
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || directory.Mode().Perm() != 0700 {
		return errors.New("private configuration directory is not owner-only")
	}
	if len(os.Args) < 3 || os.Args[1] != "--kubeconfig" || os.Args[2] != path {
		return fmt.Errorf("argument/environment boundary mismatch")
	}
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte(path), 0600); err != nil {
		return err
	}
	if os.Args[len(os.Args)-2] == "fixture-cancel" {
		time.Sleep(30 * time.Second)
		return nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return err
	}
	if cfg.AuthConfigPersister == nil {
		return fmt.Errorf("missing fixture auth persister")
	}
	return cfg.AuthConfigPersister.Persist(map[string]string{"id-token": "fake-refreshed-token"})
}

func TestEveryChildRunnerKeepsMultifileSourcesImmutable(t *testing.T) {
	root := t.TempDir()
	sources := filepath.Join(root, "sources")
	if err := os.Mkdir(sources, 0700); err != nil {
		t.Fatal(err)
	}
	operatorHome := filepath.Join(root, "operator")
	if err := os.MkdirAll(filepath.Join(operatorHome, ".kube"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(operatorHome, ".kube", ".kubeconfig"), []byte(testKubeconfig), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", operatorHome)
	targetFile := filepath.Join(root, "child-target")
	first := filepath.Join(sources, "context")
	second := filepath.Join(sources, "credential")
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["chosen"] = &clientcmdapi.Cluster{Server: "https://api.example.test"}
	cfg.Contexts["chosen"] = &clientcmdapi.Context{Cluster: "chosen", AuthInfo: "operator"}
	cfg.CurrentContext = "chosen"
	if err := clientcmd.WriteToFile(*cfg, first); err != nil {
		t.Fatal(err)
	}
	auth := clientcmdapi.NewConfig()
	auth.AuthInfos["operator"] = &clientcmdapi.AuthInfo{AuthProvider: &clientcmdapi.AuthProviderConfig{Name: "oidc", Config: map[string]string{"id-token": "fake-original-token"}}}
	original, err := clientcmd.Write(*auth)
	if err != nil {
		t.Fatal(err)
	}
	original = append([]byte("# operator formatting\n"), original...)
	firstBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(first, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first+".lock", []byte("operator-owned lock"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", first+string(os.PathListSeparator)+second)
	for _, name := range []string{"kubectl", "helm"} {
		if err := os.Symlink(os.Args[0], filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	args := []string{"--context=chosen", "--", "--kubeconfig", "/remote-only", "fixture-persist", targetFile}
	runners := map[string]func() error{
		"buffered":       func() error { _, err := DefaultRunCommand(t.Context(), nil, "kubectl", args...); return err },
		"helm":           func() error { _, err := DefaultRunHelm(t.Context(), args); return err },
		"interactive":    func() error { return DefaultRunInteractive(t.Context(), "kubectl", args...) },
		"streamed":       func() error { return DefaultRunCommandStream(t.Context(), io.Discard, "kubectl", args...) },
		"backup-stdout":  func() error { _, err := defaultRunCommandStdout(t.Context(), nil, "kubectl", args...); return err },
		"backup-io":      func() error { return defaultRunCommandIO(t.Context(), nil, io.Discard, "kubectl", args...) },
		"unseal-private": func() error { _, err := privateUnsealCommand(t.Context(), nil, "kubectl", args...); return err },
	}
	for name, run := range runners {
		if err := os.WriteFile(second, original, 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		after, err := os.ReadFile(second)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(original, after) {
			t.Fatalf("%s changed source credentials", name)
		}
		firstAfter, err := os.ReadFile(first)
		if err != nil || !bytes.Equal(firstAfter, firstBytes) {
			t.Fatal("context source changed", name, err)
		}
		for path, mode := range map[string]os.FileMode{first: 0640, second: 0600, first + ".lock": 0600} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatal("source mode changed", name, path, err)
			}
		}
		entries, err := os.ReadDir(sources)
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if !slices.Equal(names, []string{"context", "context.lock", "credential"}) {
			t.Fatal("source siblings changed", name, names)
		}
		if _, err := os.Stat(filepath.Join(operatorHome, ".kube", "config")); !os.IsNotExist(err) {
			t.Fatal("child migrated operator config", name, err)
		}
		private, err := os.ReadFile(targetFile)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(strings.TrimSpace(string(private))); !os.IsNotExist(err) {
			t.Fatal("private merge not cleaned", name, err)
		}
		t.Logf("%s: child refresh persisted only to its private merge", name)
	}
	// ModifyConfig attempts to lock even the first, context-only source file.
	if err := os.WriteFile(first+".lock", nil, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := DefaultRunCommand(t.Context(), nil, "kubectl", args...)
	if err != nil {
		t.Fatalf("child attempted source lock: %v %s", err, out)
	}
	t.Log("child persistence did not touch the locked source")
}

func TestCancelledChildCleansPrivateMergeAndKeepsSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	record := filepath.Join(root, "record")
	original := []byte(testKubeconfig)
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(os.Args[0], filepath.Join(root, "kubectl")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", source)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	_, err := DefaultRunCommand(ctx, nil, "kubectl", "--", "fixture-cancel", record)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("child cancellation lost", err)
	}
	private, err := os.ReadFile(record)
	if err != nil {
		t.Fatal("child did not reach private copy", err)
	}
	if _, err := os.Stat(string(private)); !os.IsNotExist(err) {
		t.Fatal("cancelled child left private copy", err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("cancelled child changed source", err)
	}
}
