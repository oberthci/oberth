package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

func TestChartPipelineDNSIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, namespace string
		anyDestination  bool
		extra           []string
	}{
		{name: "default", namespace: "kube-system"},
		{name: "custom", namespace: "dns-system", extra: []string{"--set", "networkPolicy.dnsNamespace=dns-system"}},
		{name: "host-network escape hatch", anyDestination: true, extra: []string{"--set", "networkPolicy.dnsAnyDestination=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policies, _ := renderGoProxyPolicies(t, 8444, tc.extra...)
			pipeline := chartPolicyEndpoint{namespace: "pipeline-system"}
			for _, namespace := range []string{"kube-system", "dns-system", "other-system", ""} {
				want := tc.anyDestination || namespace == tc.namespace
				if got := chartPolicyAllows(t, policies, networkingv1.PolicyTypeEgress, pipeline, chartPolicyEndpoint{namespace: namespace}, 53); got != want {
					t.Errorf("DNS to %q: got %v, want %v", namespace, got, want)
				}
			}
			protocols := map[corev1.Protocol]bool{}
			for _, policy := range policies {
				if policy.Namespace != pipeline.namespace {
					continue
				}
				for _, rule := range policy.Spec.Egress {
					for _, port := range rule.Ports {
						if port.Port != nil && port.Port.IntVal == 53 && port.Protocol != nil {
							protocols[*port.Protocol] = true
						}
					}
				}
			}
			if !protocols[corev1.ProtocolUDP] || !protocols[corev1.ProtocolTCP] {
				t.Fatalf("DNS protocols missing: %v", protocols)
			}
		})
	}
}

func TestChartPipelineIngressIsolation(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		var extra []string
		if !enabled {
			extra = []string{"--set", "networkPolicy.pipelineIngress.enabled=false"}
		}
		policies, server := renderGoProxyPolicies(t, 8444, extra...)
		pipeline := chartPolicyEndpoint{namespace: "pipeline-system"}
		for _, tc := range []struct {
			name    string
			source  chartPolicyEndpoint
			allowed bool
		}{
			{"pipeline peer", pipeline, true},
			{"this release server", server, true},
			{"unrelated server namespace pod", chartPolicyEndpoint{namespace: server.namespace}, false},
			{"server labels in another namespace", chartPolicyEndpoint{namespace: "other-system", labels: server.labels}, false},
			{"unrelated namespace pod", chartPolicyEndpoint{namespace: "other-system"}, false},
			{"external client", chartPolicyEndpoint{}, false},
		} {
			for _, port := range []int32{80, 8444, 3000} {
				if got := chartPolicyAllows(t, policies, networkingv1.PolicyTypeIngress, tc.source, pipeline, port); got != (!enabled || tc.allowed) {
					t.Errorf("enabled=%v %s port=%d allowed=%v", enabled, tc.name, port, got)
				}
			}
		}
	}
}
