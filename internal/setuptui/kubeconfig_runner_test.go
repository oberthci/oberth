package setuptui

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func init() {
	name := filepath.Base(os.Args[0])
	if name != "kubectl" && name != "helm" {
		return
	}
	// A test-only executable symlink reaches this before TestMain isolates HOME.
	if err := persistWizardChild(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func persistWizardChild() error {
	path := os.Getenv("KUBECONFIG")
	if len(os.Args) < 3 || os.Args[1] != "--kubeconfig" || os.Args[2] != path {
		return fmt.Errorf("argument/environment target mismatch")
	}
	for p, want := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if st.Mode().Perm() != want {
			return fmt.Errorf("private target permissions: got %04o want %04o", st.Mode().Perm(), want)
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	raw, err := loader.RawConfig()
	if err != nil {
		return err
	}
	config, err := loader.ClientConfig()
	if err != nil {
		return err
	}
	if raw.CurrentContext != "chosen" || config.AuthConfigPersister == nil {
		return fmt.Errorf("selected context/persister missing")
	}
	record := raw.AuthInfos["operator"].AuthProvider.Config["record-path"]
	if err := os.WriteFile(record, []byte(path), 0600); err != nil {
		return err
	}
	return config.AuthConfigPersister.Persist(map[string]string{"id-token": "fixture-refreshed"})
}

func TestSetupChildrenPreserveSourcesAndCleanupPrivateCopies(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "operator")
	dir := filepath.Join(root, "sources")
	for _, p := range []string{filepath.Join(home, ".kube"), dir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	first, second := filepath.Join(dir, "context"), filepath.Join(dir, "credential")
	record := filepath.Join(root, "record")
	raw := clientcmdapi.NewConfig()
	raw.CurrentContext = "chosen"
	raw.Clusters["chosen"] = &clientcmdapi.Cluster{Server: "https://api.example.test"}
	raw.Contexts["chosen"] = &clientcmdapi.Context{Cluster: "chosen", AuthInfo: "operator"}
	if err := clientcmd.WriteToFile(*raw, first); err != nil {
		t.Fatal(err)
	}
	auth := clientcmdapi.NewConfig()
	auth.AuthInfos["operator"] = &clientcmdapi.AuthInfo{AuthProvider: &clientcmdapi.AuthProviderConfig{Name: "oidc", Config: map[string]string{"id-token": "fixture-original", "record-path": record}}}
	if err := clientcmd.WriteToFile(*auth, second); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(first, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first+".lock", []byte("operator-owned"), 0600); err != nil {
		t.Fatal(err)
	}
	original := map[string][]byte{}
	modes := map[string]os.FileMode{}
	for _, p := range []string{first, second, first + ".lock"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		original[p] = b
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		modes[p] = st.Mode().Perm()
	}
	migration := filepath.Join(home, ".kube", ".kubeconfig")
	if err := os.WriteFile(migration, original[first], 0600); err != nil {
		t.Fatal(err)
	}
	siblings, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kubectl", "helm"} {
		if err := os.Symlink(os.Args[0], filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KUBECONFIG", first+string(os.PathListSeparator)+second)
	for _, name := range []string{"kubectl", "helm"} {
		t.Run(name, func(t *testing.T) {
			if name == "kubectl" {
				if _, err := defaultRunKubectl(t.Context(), "--context", "chosen", "get", "pods"); err != nil {
					t.Fatal(err)
				}
			} else {
				present, known := defaultReleasePresent(t.Context(), "", "chosen", "oberth")
				if !present || !known {
					t.Fatal("release probe failed")
				}
			}
			private, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(string(private)); !os.IsNotExist(err) {
				t.Fatalf("private copy retained: %v", err)
			}
			for p, before := range original {
				after, err := os.ReadFile(p)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("source changed: %s %v", p, err)
				}
				st, err := os.Stat(p)
				if err != nil || st.Mode().Perm() != modes[p] {
					t.Fatalf("source permissions changed: %s", p)
				}
			}
			after, err := os.ReadDir(dir)
			if err != nil || !reflect.DeepEqual(siblings, after) {
				t.Fatal("source sibling set changed", err)
			}
			if _, err := os.Stat(filepath.Join(home, ".kube", "config")); !os.IsNotExist(err) {
				t.Fatal("default migration destination created", err)
			}
			afterMigration, err := os.ReadFile(migration)
			if err != nil || !bytes.Equal(afterMigration, original[first]) {
				t.Fatal("migration source changed", err)
			}
		})
	}
}
