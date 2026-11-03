package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
)

type chartPolicyEndpoint struct {
	namespace string // Empty for a client outside the cluster.
	labels    map[string]string
	ports     map[string]int32
}

// Render the whole chart so an allowance in any policy participates in the
// union. Resolve named policy ports against the actual server container ports.
func renderGoProxyPolicies(t *testing.T, proxyPort int32) ([]networkingv1.NetworkPolicy, chartPolicyEndpoint) {
	t.Helper()
	out, err := exec.Command("helm", "template", "policy-test", "../../charts/oberth",
		"--namespace", "server-system",
		"--set", "image.ref=example.invalid/oberth@"+goProxyDigest,
		"--set", "argo.namespace=pipeline-system",
		"--set", "argo.goProxy.enabled=true",
		"--set", fmt.Sprintf("argo.goProxy.port=%d", proxyPort),
		"--set", "argo.goProxy.modulePrefix=go.example.test",
		"--set", "argo.goProxy.upstream=forge",
		"--set", "argo.goProxy.organization=example",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("render chart: %v\n%s", err, out)
	}
	var policies []networkingv1.NetworkPolicy
	var server chartPolicyEndpoint
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 {
			continue
		}
		var meta metav1.TypeMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "NetworkPolicy":
			var policy networkingv1.NetworkPolicy
			if err := json.Unmarshal(raw, &policy); err != nil {
				t.Fatal(err)
			}
			policies = append(policies, policy)
		case "Deployment":
			var deployment appsv1.Deployment
			if err := json.Unmarshal(raw, &deployment); err != nil {
				t.Fatal(err)
			}
			if deployment.Name != "policy-test-oberth" {
				continue
			}
			server = chartPolicyEndpoint{namespace: deployment.Namespace,
				labels: deployment.Spec.Template.Labels, ports: map[string]int32{}}
			for _, container := range deployment.Spec.Template.Spec.Containers {
				for _, port := range container.Ports {
					server.ports[port.Name] = port.ContainerPort
				}
			}
		}
	}
	if len(policies) == 0 || server.ports["ssh"] != 2222 || server.ports["https"] != 8443 || server.ports["goproxy"] != proxyPort {
		t.Fatalf("missing policies or unexpected server listeners: policies=%d, server=%+v", len(policies), server)
	}
	return policies, server
}

func chartSelectorMatches(t *testing.T, selector *metav1.LabelSelector, values map[string]string) bool {
	t.Helper()
	parsed, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Matches(labels.Set(values))
}

// This evaluator covers the chart's TCP pod/namespace rules, not CNI routing,
// node exceptions, Service DNAT or IPBlocks. Fail on unsupported peer types.
func chartPolicyAllows(t *testing.T, policies []networkingv1.NetworkPolicy, direction networkingv1.PolicyType, src, dst chartPolicyEndpoint, port int32) bool {
	t.Helper()
	selected, peer := dst, src
	if direction == networkingv1.PolicyTypeEgress {
		selected, peer = src, dst
	}
	isolated, allowed := false, false
	for _, policy := range policies {
		if policy.Namespace != selected.namespace || !chartSelectorMatches(t, &policy.Spec.PodSelector, selected.labels) {
			continue
		}
		types := policy.Spec.PolicyTypes
		if len(types) == 0 {
			types = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
			if len(policy.Spec.Egress) > 0 {
				types = append(types, networkingv1.PolicyTypeEgress)
			}
		}
		if !slices.Contains(types, direction) {
			continue
		}
		isolated = true
		if direction == networkingv1.PolicyTypeIngress {
			for _, rule := range policy.Spec.Ingress {
				allowed = chartPolicyRuleAllows(t, policy.Namespace, rule.From, rule.Ports, peer, dst, port) || allowed
			}
		} else {
			for _, rule := range policy.Spec.Egress {
				allowed = chartPolicyRuleAllows(t, policy.Namespace, rule.To, rule.Ports, peer, dst, port) || allowed
			}
		}
	}
	return !isolated || allowed
}

func chartPolicyRuleAllows(t *testing.T, policyNamespace string, peers []networkingv1.NetworkPolicyPeer, ports []networkingv1.NetworkPolicyPort, peer, dst chartPolicyEndpoint, port int32) bool {
	t.Helper()
	portAllowed := len(ports) == 0
	for _, candidate := range ports {
		if candidate.Protocol != nil && *candidate.Protocol != corev1.ProtocolTCP {
			continue
		}
		if candidate.Port == nil {
			portAllowed = true
		} else if candidate.Port.Type == intstr.String {
			portAllowed = portAllowed || dst.ports[candidate.Port.StrVal] == port
		} else if candidate.EndPort != nil {
			portAllowed = portAllowed || port >= candidate.Port.IntVal && port <= *candidate.EndPort
		} else {
			portAllowed = portAllowed || candidate.Port.IntVal == port
		}
	}
	if !portAllowed {
		return false
	}
	if len(peers) == 0 {
		return true
	}
	for _, candidate := range peers {
		if candidate.IPBlock != nil {
			t.Fatal("IPBlock evaluation is outside these chart pod-policy fixtures")
		}
		if candidate.NamespaceSelector == nil && candidate.PodSelector == nil {
			return true
		}
		if peer.namespace == "" {
			continue
		}
		if candidate.NamespaceSelector != nil {
			if !chartSelectorMatches(t, candidate.NamespaceSelector, map[string]string{"kubernetes.io/metadata.name": peer.namespace}) {
				continue
			}
		} else if peer.namespace != policyNamespace {
			continue
		}
		if candidate.PodSelector == nil || chartSelectorMatches(t, candidate.PodSelector, peer.labels) {
			return true
		}
	}
	return false
}

func TestChartGoProxyEffectiveIngressUnion(t *testing.T) {
	for _, proxyPort := range []int32{8444, 9444} {
		t.Run(fmt.Sprint(proxyPort), func(t *testing.T) {
			policies, server := renderGoProxyPolicies(t, proxyPort)
			for _, client := range []struct {
				name     string
				endpoint chartPolicyEndpoint
				proxy    bool
			}{
				{"external", chartPolicyEndpoint{}, false},
				{"ingress or tunnel pod", chartPolicyEndpoint{namespace: "ingress-system"}, false},
				{"pipeline", chartPolicyEndpoint{namespace: "pipeline-system", labels: map[string]string{"oberth.ci/trigger": "branch"}}, true},
				{"unlabeled pipeline pod", chartPolicyEndpoint{namespace: "pipeline-system"}, false},
				{"trigger label in wrong namespace", chartPolicyEndpoint{namespace: "other-system", labels: map[string]string{"oberth.ci/trigger": "branch"}}, false},
			} {
				t.Run(client.name, func(t *testing.T) {
					for _, listener := range []string{"ssh", "https", "goproxy"} {
						want := listener != "goproxy" || client.proxy
						if got := chartPolicyAllows(t, policies, networkingv1.PolicyTypeIngress, client.endpoint, server, server.ports[listener]); got != want {
							t.Errorf("effective ingress to %s: got %v, want %v", listener, got, want)
						}
					}
				})
			}
		})
	}
}

func TestChartGoProxyEffectivePipelineAccess(t *testing.T) {
	for _, proxyPort := range []int32{8444, 9444} {
		t.Run(fmt.Sprint(proxyPort), func(t *testing.T) {
			policies, server := renderGoProxyPolicies(t, proxyPort)
			pipeline := chartPolicyEndpoint{namespace: "pipeline-system", labels: map[string]string{"oberth.ci/trigger": "branch"}}
			for _, listener := range []string{"goproxy", "https", "ssh"} {
				t.Run(listener, func(t *testing.T) {
					port := server.ports[listener]
					ingress := chartPolicyAllows(t, policies, networkingv1.PolicyTypeIngress, pipeline, server, port)
					egress := chartPolicyAllows(t, policies, networkingv1.PolicyTypeEgress, pipeline, server, port)
					want := listener == "goproxy"
					if !ingress || egress != want || (ingress && egress) != want {
						t.Errorf("pipeline to %s on %d: ingress=%v, egress=%v, want connection=%v", listener, port, ingress, egress, want)
					}
				})
			}
			// The namespace and pod selectors must apply together, and the
			// instance label must distinguish other releases in the same namespace.
			for _, target := range []struct {
				name     string
				endpoint chartPolicyEndpoint
			}{
				{"matching labels in another namespace", chartPolicyEndpoint{namespace: "other-system", labels: server.labels}},
				{"other release in server namespace", chartPolicyEndpoint{namespace: server.namespace, labels: map[string]string{"app.kubernetes.io/name": "oberth", "app.kubernetes.io/instance": "other-release"}}},
				{"unrelated pod in server namespace", chartPolicyEndpoint{namespace: server.namespace}},
			} {
				t.Run(target.name, func(t *testing.T) {
					if chartPolicyAllows(t, policies, networkingv1.PolicyTypeEgress, pipeline, target.endpoint, proxyPort) {
						t.Error("proxy egress escapes the configured server destination")
					}
				})
			}
			// Egress alone does not grant unlabeled pipeline pods proxy access.
			unlabeled := chartPolicyEndpoint{namespace: pipeline.namespace}
			if chartPolicyAllows(t, policies, networkingv1.PolicyTypeIngress, unlabeled, server, proxyPort) &&
				chartPolicyAllows(t, policies, networkingv1.PolicyTypeEgress, unlabeled, server, proxyPort) {
				t.Error("unlabeled pipeline pod can reach the proxy")
			}
			if !chartPolicyAllows(t, policies, networkingv1.PolicyTypeEgress, pipeline, chartPolicyEndpoint{}, 443) {
				t.Error("ordinary outbound HTTPS was lost")
			}
		})
	}
}
