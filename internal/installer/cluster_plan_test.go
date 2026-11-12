package installer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func preflightNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{"k3s.io/node-args": `["server"]`}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{Architecture: "arm64", KubeletVersion: "v1.35.0+k3s1"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
}
func preflightStorage() *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "default", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}}
}
func preflightStats(context.Context, []byte, string, ...string) ([]byte, error) {
	return []byte(`{"node":{"fs":{"availableBytes":4294967296}}}`), nil
}

func TestPreflightReportsFactsAndRemedies(t *testing.T) {
	node := preflightNode()
	kube := fake.NewClientset(node, preflightStorage())
	deps := Deps{KubeClient: kube, ContextName: "chosen", RunCommand: preflightStats}
	identity := ClusterIdentity{}
	result := InstallResult{OK: true}
	inspectClusterPreflight(context.Background(), &Config{}, deps, &identity, &result)
	if !result.OK || len(identity.Nodes) != 1 || identity.Nodes[0].AvailableBytes == nil || identity.Distribution != "k3s" {
		t.Fatalf("preflight facts missing: %v", result.Checks)
	}
	node.Annotations["k3s.io/node-args"] = `["server","--disable-network-policy"]`
	node.Status.NodeInfo.Architecture = "unsupported"
	if _, err := kube.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) {
		return nil, errors.New("private diagnostic")
	}
	result = InstallResult{OK: true}
	inspectClusterPreflight(context.Background(), &Config{}, deps, &ClusterIdentity{}, &result)
	failed := map[string]bool{}
	for _, check := range result.Checks {
		if !check.OK {
			failed[check.Code] = true
			if check.Remedy == "" || strings.Contains(check.Remedy, "private diagnostic") {
				t.Fatal("unsafe remedy")
			}
		}
	}
	if result.OK || !failed["network_policy"] || !failed["disk/node"] || !failed["architecture/node"] {
		t.Fatal("preflight accepted unsupported node")
	}
}

func TestMultifileKubeconfigKeepsExplicitContext(t *testing.T) {
	bootstrapCredentialInventory(t)
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	if err := os.WriteFile(first, []byte(testKubeconfig), 0600); err != nil {
		t.Fatal(err)
	}
	other := strings.ReplaceAll(testKubeconfig, "test", "other")
	other = strings.ReplaceAll(other, "127.0.0.1", "192.0.2.10")
	if err := os.WriteFile(second, []byte(other), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", first+string(os.PathListSeparator)+second)
	names, err := KubeconfigContexts("")
	if err != nil || len(names) != 2 {
		t.Fatal("multifile contexts not merged")
	}
	_, config, selected, err := LoadKubeConfigForContext("other", io.Discard)
	if err != nil || selected != "other" || config.Host != "https://192.0.2.10:6443" {
		t.Fatal("explicit context changed")
	}
	args, _, err := ResolveHelmKubeArgs("other")
	if err != nil || strings.Join(args, " ") != "--kube-context other" {
		t.Fatal("Helm target differs")
	}
	if _, _, _, err := LoadKubeConfigForContext("missing", io.Discard); err == nil {
		t.Fatal("unknown context accepted")
	}
	if err := RequireContextChoice("install on", "", "", KubeconfigContexts); err == nil {
		t.Fatal("ambiguous context accepted")
	}
}

func TestDarwinUsesSelectedKubeconfigWithoutCreatingKind(t *testing.T) {
	called := false
	deps := InstallDeps{GOOS: "darwin", LookPath: func(name string) (string, error) { return name, nil }, RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("implicit host mutation")
		return nil, nil
	}, LoadKubeConfig: func(name string) (kubernetes.Interface, *rest.Config, string, error) {
		called = true
		if name != "chosen" {
			t.Fatal("context changed")
		}
		return nil, nil, "", errors.New("selected context reached")
	}}
	err := Execute(context.Background(), Config{ContextName: "chosen"}, deps)
	if !called || err == nil || !strings.Contains(err.Error(), "selected context reached") {
		t.Fatal("user context not selected")
	}
}

func TestFreshK3sNeverUsedWhenUserFileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	rules := &clientcmd.ClientConfigLoadingRules{Precedence: []string{path}}
	if err := freshK3sHint(rules, path); err != nil {
		t.Fatal("existing user configuration replaced")
	}
}

func TestPlanReportsSelectedRemoteClusterWithoutAnotherGate(t *testing.T) {
	bootstrapCredentialInventory(t)
	ca := newTestServerCA(t, []string{"api.example.test"})
	kube := fake.NewClientset(preflightNode(), preflightStorage())
	deps := InstallDeps{LookPath: func(name string) (string, error) { return name, nil }, LoadKubeConfig: func(name string) (kubernetes.Interface, *rest.Config, string, error) {
		if name != "chosen" {
			t.Fatal("context changed")
		}
		return kube, &rest.Config{Host: "https://api.example.test:6443", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, name, nil
	}, RunCommand: preflightStats}
	plan := PlanInstall(context.Background(), Config{ContextName: "chosen"}, deps)
	if !plan.OK || plan.Identity.Context != "chosen" || plan.Identity.CAPin == "" || plan.Identity.Server != "https://api.example.test:6443" || len(plan.Identity.Nodes) != 1 {
		t.Fatalf("plan failed: %v", plan.Checks)
	}
	for _, check := range plan.Checks {
		if strings.Contains(check.Code, "remote") {
			t.Fatal("remote cluster gate introduced")
		}
	}
}

func TestExecuteRefusesUnknownNetworkPolicyBeforeHelm(t *testing.T) {
	node := preflightNode()
	node.Annotations = nil
	kube := fake.NewClientset(node, preflightStorage())
	deps := InstallDeps{Output: io.Discard, LookPath: func(name string) (string, error) { return name, nil }, LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
		return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: newTestServerCA(t, []string{"api.example.test"})}}, "chosen", nil
	}, RunCommand: preflightStats, RunHelm: func(context.Context, []string) ([]byte, error) {
		t.Fatal("Helm ran without network-policy enforcement")
		return nil, nil
	}}
	err := Execute(context.Background(), Config{ContextName: "chosen"}, deps)
	if !errors.Is(err, errInstallPreflight) || !strings.Contains(err.Error(), "network_policy:") {
		t.Fatal("unknown enforcement was accepted")
	}
}

func TestVerifyRequiresUpstreamAuthentication(t *testing.T) {
	for _, test := range []struct {
		raw string
		ok  bool
	}{
		{"NAME  KIND URL                           AUTH\nforge ssh  ssh://git@forge.example/acme   ok\n", true},
		{"NAME  KIND URL                           AUTH\nforge ssh  ssh://git@forge.example/acme   REJECTED\n", false},
		{"NAME KIND URL\nforge ssh ssh://git@forge.example/acme\n", false},
		{"NAME KIND URL AUTH\n", false},
	} {
		if verifiedUpstreams([]byte(test.raw)) != test.ok {
			t.Fatal("upstream authentication result was misclassified")
		}
	}
}

func TestPolicyPreflightReadsControllerConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, controller string
		args             []string
		config           map[string]string
		ready            int32
		nodeCount        int
		want             bool
	}{
		{name: "cilium enabled", controller: "cilium", config: map[string]string{"enable-policy": "default"}, ready: 1, want: true},
		{name: "cilium disabled", controller: "cilium", config: map[string]string{"enable-policy": "never"}, ready: 1},
		{name: "cilium audit only", controller: "cilium", config: map[string]string{"policy-audit-mode": "true"}, ready: 1},
		{name: "cilium k8s policies disabled", controller: "cilium", config: map[string]string{"enable-k8s-networkpolicy": "false"}, ready: 1},
		{name: "cilium argument overrides config", controller: "cilium", args: []string{"--enable-policy", "never"}, config: map[string]string{"enable-policy": "default"}, ready: 1},
		{name: "router firewall enabled", controller: "kube-router", args: []string{"--run-firewall=true"}, ready: 1, want: true},
		{name: "router firewall disabled", controller: "kube-router", args: []string{"--run-firewall=false"}, ready: 1},
		{name: "router split disabled flag", controller: "kube-router", args: []string{"--run-firewall", "false"}, ready: 1},
		{name: "controller does not cover nodes", controller: "kube-router", ready: 1, nodeCount: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := preflightNode()
			node.Annotations = nil
			ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: test.controller, Namespace: "networking", Generation: 1},
				Spec:   appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: test.controller, Args: test.args}}}}},
				Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: test.ready, NumberReady: test.ready, UpdatedNumberScheduled: test.ready}}
			config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: "networking"}, Data: test.config}
			kube := fake.NewClientset(node, preflightStorage(), ds, config)
			if test.nodeCount > 1 {
				other := node.DeepCopy()
				other.Name = "node-two"
				if _, err := kube.CoreV1().Nodes().Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			result := InstallResult{OK: true}
			inspectClusterPreflight(context.Background(), &Config{}, Deps{KubeClient: kube, RunCommand: preflightStats}, &ClusterIdentity{}, &result)
			if result.OK != test.want {
				t.Fatalf("preflight ok=%t, want %t", result.OK, test.want)
			}
		})
	}
}

func TestAllInstallModesRefuseUntrustedClusterBeforeAccess(t *testing.T) {
	ca := newTestServerCA(t, []string{"api.example.test"})
	for _, config := range []*rest.Config{
		{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca, Insecure: true}},
		{Host: "https://api.example.test"},
		{Host: "http://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}},
	} {
		kube := fake.NewClientset()
		deps := InstallDeps{Output: io.Discard, LookPath: func(name string) (string, error) { return name, nil },
			LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) { return kube, config, "chosen", nil },
			RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
				t.Fatal("subprocess contacted an untrusted cluster")
				return nil, nil
			},
			RunHelm: func(context.Context, []string) ([]byte, error) {
				t.Fatal("Helm contacted an untrusted cluster")
				return nil, nil
			}}
		cfg := Config{ContextName: "chosen"}
		if PlanInstall(context.Background(), cfg, deps).OK || VerifyInstall(context.Background(), cfg, deps).OK {
			t.Fatal("read-only mode accepted an untrusted cluster")
		}
		if err := Execute(context.Background(), cfg, deps); !errors.Is(err, errInstallPreflight) || !strings.Contains(err.Error(), "cluster_ca:") {
			t.Fatal("install bypassed the plan's trust check")
		}
		if len(kube.Actions()) != 0 {
			t.Fatal("credentials could have been sent before refusing untrusted cluster")
		}
	}
}
