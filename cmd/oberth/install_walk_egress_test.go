package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

func TestWalkInstallReviewsVMSSHAllowance(t *testing.T) {
	for _, cidr := range []string{"10.42.0.0/24", "fd00:42::/64"} {
		for _, format := range []string{"text", "json"} {
			t.Run(cidr+"/"+format, func(t *testing.T) {
				checkWalkInstallSSHAllowance(t, cidr, format)
			})
		}
	}
}

func checkWalkInstallSSHAllowance(t *testing.T, cidr, format string) {
	t.Helper()
	engine, charts := offlineInstallEngine(t, t.TempDir())
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "walk-node"}, Spec: corev1.NodeSpec{PodCIDRs: []string{cidr}}}
	if _, err := engine.Cluster.Client.CoreV1().Nodes().Create(t.Context(), node, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	flags := installationFlags("install")
	args := append(walkInstallEgress(t, cidr), "--context=chosen", "--chart=../../charts/oberth", "--dry-run", "--output="+format)
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	request := installationRequest{name: "install", flags: flags}
	session := installationSession{engine: engine, request: request, choices: request.choices()}
	var out bytes.Buffer
	if err := session.applyInstallation(t.Context(), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	visible := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"Egress allow: " + cidr + " TCP/22", "Run egress: internet open, internal closed"} {
		if !strings.Contains(visible, want) {
			t.Fatal("walk install hid its operator exception from Review", out.String())
		}
	}
	if len(charts.Previews) != 1 || len(charts.Applies) != 0 {
		t.Fatal("walk review skipped preparation or applied changes")
	}
	assertWalkSSHRule(t, charts.Previews[0], cidr)
}

// Exercise the operator commands beside the walk's VM resources, so omitting
// the install exception cannot pass merely because the product supports it.
func walkInstallEgress(t *testing.T, cidr string) []string {
	t.Helper()
	body, err := os.ReadFile("../../hack/walk-vm-resources.yaml")
	if err != nil {
		t.Fatal(err)
	}
	allowances := map[string]string{}
	for _, command := range []string{"install", "upgrade"} {
		prefix := "# oberth " + command + " --egress-allow '"
		for line := range strings.SplitSeq(string(body), "\n") {
			if raw, found := strings.CutPrefix(line, prefix); found {
				raw, quoted := strings.CutSuffix(raw, "'")
				if !quoted || allowances[command] != "" {
					t.Fatal("walk egress command is ambiguous", command)
				}
				allowances[command] = strings.ReplaceAll(raw, "REPLACE_WITH_CLUSTER_POD_CIDR", cidr)
			}
		}
		if allowances[command] == "" {
			t.Fatal("walk " + command + " lacks its explicit --egress-allow command")
		}
	}
	if allowances["install"] != allowances["upgrade"] {
		t.Fatal("walk install and upgrade name different operator exceptions")
	}
	return []string{"--egress-allow", allowances["install"]}
}

func assertWalkSSHRule(t *testing.T, request installer.ChartRequest, cidr string) {
	t.Helper()
	files, err := installer.Render(request.Chart, request.Values, request.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(strings.NewReader(files["oberth/templates/pipeline.yaml"]), 4096)
	for {
		var policy networkingv1.NetworkPolicy
		if err = decoder.Decode(&policy); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if policy.Kind == "NetworkPolicy" && policy.Name == "oberth-egress" {
			assertWalkSSHPeers(t, policy, cidr)
			return
		}
	}
	t.Fatal("walk install lost its rendered egress policy")
}

func assertWalkSSHPeers(t *testing.T, policy networkingv1.NetworkPolicy, cidr string) {
	t.Helper()
	count := 0
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock == nil || peer.IPBlock.CIDR != cidr {
				continue
			}
			count++
			if len(rule.To) != 1 || len(peer.IPBlock.Except) != 0 || len(rule.Ports) != 1 {
				t.Fatal("walk SSH exception changed its destination or ports", rule)
			}
			assertWalkSSHPort(t, rule.Ports[0])
		}
	}
	if count != 1 {
		t.Fatal("walk Pod CIDR has no unique explicit SSH rule", count)
	}
}

func assertWalkSSHPort(t *testing.T, port networkingv1.NetworkPolicyPort) {
	t.Helper()
	if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP || port.Port == nil || *port.Port != intstr.FromInt32(22) || port.EndPort != nil {
		t.Fatal("walk SSH exception is not exactly TCP 22", port)
	}
}
