package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestSingleHostNamesFollowSelectedNode(t *testing.T) {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node.example.test"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: "node"}, {Type: corev1.NodeInternalIP, Address: "192.0.2.10"}, {Type: corev1.NodeInternalIP, Address: "2001:db8::10"}, {Type: corev1.NodeInternalIP, Address: "0.0.0.0"}}}}
	dns, ips := SingleHostTLSNames([]corev1.Node{node}, []string{"extra.example.test"}, nil)
	for _, name := range []string{"localhost", "node", "node.local", "node.example.test", "extra.example.test"} {
		if !slices.Contains(dns, name) {
			t.Fatalf("missing DNS name %s", name)
		}
	}
	for _, ip := range []string{"127.0.0.1", "::1", "192.0.2.10", "2001:db8::10"} {
		if !slices.Contains(ips, ip) {
			t.Fatalf("missing IP %s", ip)
		}
	}
	if slices.Contains(ips, "0.0.0.0") {
		t.Fatal("listen address included")
	}
	againDNS, againIPs := SingleHostTLSNames([]corev1.Node{node}, dns, ips)
	if !slices.Equal(dns, againDNS) || !slices.Equal(ips, againIPs) {
		t.Fatal("discovery is not idempotent")
	}
	dns, ips = SingleHostTLSNames([]corev1.Node{node, node}, nil, nil)
	if len(dns)+len(ips) != 0 {
		t.Fatal("single-host names added to multiple nodes")
	}
}

func TestExistingCARequiresExplicitReissue(t *testing.T) {
	cfg := Config{Namespace: DefaultNamespace, TLSExtraDNSNames: []string{"new.example.test"}}
	base, _ := serverPKINames(Config{})
	issuer, _ := json.Marshal(map[string]any{"data": map[string]any{"certificate": string(newTestServerCA(base))}})
	responses := freshStoreResponses()
	responses["read -format=json oberth-pki/issuer/default"] = fakeBaoResponse{out: string(issuer)}
	runner := &fakeBaoRunner{t: t, responses: responses}
	_, _, err := ensureServerPKI(t.Context(), cfg, newOpenBaoExec(secretStoreTestDeps(runner, io.Discard), DefaultOpenBaoNamespace, "openbao-0"), "test-token", map[string]baoMount{"oberth-pki/": {Type: "pki"}})
	if err == nil || !strings.Contains(err.Error(), "--reissue-server-ca") {
		t.Fatal("CA silently widened")
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call.command, "write ") {
			t.Fatal("CA or role changed before confirmation")
		}
	}
}

func TestExistingIPConstrainedCANeverDowngrades(t *testing.T) {
	pem := newTestServerCA([]string{"oberth", "localhost"}, "127.0.0.1", "::1")
	current := map[string]any{"certificate": string(pem)}
	cfg := Config{ReissueServerCA: true}
	kept, err := selectExistingServerCA(cfg, current, []string{"oberth", "localhost"}, []string{"127.0.0.1/32", "::1/128"})
	if err != nil || kept == "" {
		t.Fatal("matching constrained CA was not kept")
	}
	for _, names := range [][]string{{"oberth", "new.example.test"}, {"oberth"}} {
		_, err = selectExistingServerCA(cfg, current, names, []string{"192.0.2.2/32"})
		if err == nil || !strings.Contains(err.Error(), "cannot preserve") {
			t.Fatal("IP-constrained CA could be replaced by DNS-only generator")
		}
	}
}

func TestInstallJSONKeepsDiscoveredNamesAfterPreflightRefusal(t *testing.T) {
	var output bytes.Buffer
	kube := fake.NewClientset(preflightNode(), preflightStorage())
	deps := InstallDeps{Output: &output, LookPath: func(name string) (string, error) { return name, nil },
		LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
			return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: newTestServerCA([]string{"api.example.test"})}}, "chosen", nil
		}, RunCommand: preflightStats,
		RunHelm: func(context.Context, []string) ([]byte, error) {
			t.Fatal("Helm ran after failed preflight")
			return nil, nil
		}}
	if err := ExecuteJSON(t.Context(), Config{ContextName: "chosen", NetworkPolicy: "false"}, deps); err == nil {
		t.Fatal("disabled isolation was accepted")
	}
	var result InstallResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Review == nil || !slices.Contains(result.Review.DNSNames, "node.local") || !slices.Contains(result.Review.IPs, "::1") {
		t.Fatal("JSON discarded the cluster's discovered certificate names")
	}
}
