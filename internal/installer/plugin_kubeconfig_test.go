package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestPluginPlanAndInstallKeepKubeconfigBytes(t *testing.T) {
	root := t.TempDir()
	paths := []string{filepath.Join(root, "cloud-config"), filepath.Join(root, "kind-config")}
	originals := map[string][]byte{}
	ca := newTestServerCA(t, []string{"api.example.test"})
	for i, name := range []string{"cloud", "kind-target"} {
		config := clientcmdapi.NewConfig()
		config.Clusters[name] = &clientcmdapi.Cluster{Server: "https://api.example.test:6443", CertificateAuthorityData: ca}
		config.Contexts[name] = &clientcmdapi.Context{Cluster: name, Namespace: "retained-namespace"}
		config.CurrentContext = name
		raw, err := clientcmd.Write(*config)
		if err != nil {
			t.Fatal(err)
		}
		raw = append([]byte("# Keep this operator comment and exact formatting.\n"), raw...)
		if err := os.WriteFile(paths[i], raw, 0600); err != nil {
			t.Fatal(err)
		}
		originals[paths[i]] = raw
	}
	t.Setenv("KUBECONFIG", strings.Join(paths, string(os.PathListSeparator)))
	check := func(stage string) {
		t.Helper()
		for path, want := range originals {
			raw, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, want) {
				t.Fatalf("%s rewrote %s: %v", stage, path, err)
			}
		}
		contexts, err := KubeconfigContexts("")
		if err != nil || !slices.Equal(contexts, []string{"cloud", "kind-target"}) {
			t.Fatalf("%s lost a context: %v %v", stage, contexts, err)
		}
	}
	kube := fake.NewClientset(preflightNode(), preflightStorage())
	helmCalled := false
	stop := errors.New("stop at Helm after the actual install preflight")
	host := testAdminInstallDeps(InstallDeps{Output: io.Discard, CredentialSink: func(string, string) {}, LookPath: func(name string) (string, error) { return name, nil }, KubeContexts: KubeconfigContexts,
		LoadKubeConfig: func(name string) (kubernetes.Interface, *rest.Config, string, error) {
			_, config, selected, err := LoadKubeConfigForContext(name, io.Discard)
			if name != "kind-target" || selected != name {
				t.Fatal("selected context changed")
			}
			if config != nil && config.AuthConfigPersister != nil {
				t.Fatal("kubeconfig persistence is enabled")
			}
			return kube, config, selected, err
		}, ProbeNetworkPolicy: enforcedPolicyFixture, RunCommand: preflightStats,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if strings.Contains(" "+strings.Join(args, " ")+" ", " list ") {
				return []byte("[]"), nil
			}
			helmCalled = true
			return nil, stop
		},
	})
	cfg := Config{ContextName: "kind-target", InstallSecretStoreDev: true, ChartPath: testLocalChart, ClientAccess: "none"}
	plan := PlanInstall(t.Context(), cfg, host)
	if !plan.OK || plan.Identity.Context != "kind-target" {
		t.Fatalf("plan: %+v", plan)
	}
	check("plan")
	err := Execute(t.Context(), cfg, host)
	if err == nil || !helmCalled {
		t.Fatalf("install did not reach Helm after preflight: %v", err)
	}
	check("install")
}
