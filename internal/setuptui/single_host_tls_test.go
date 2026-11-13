package setuptui

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/installer"
)

func TestChangingClusterDoesNotRetainDiscoveredTLSNames(t *testing.T) {
	previous := clusterProbe
	t.Cleanup(func() { clusterProbe = previous })
	clusterProbe = func(contextName, _ string) clusterInfoMsg {
		return clusterInfoMsg{context: contextName, nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: contextName + ".example.test"}}}}
	}
	state := &WizardState{Config: installer.Config{TLSExtraDNSNames: []string{"extra.example.test"}}}
	state.done[stepTLS] = true
	for _, contextName := range []string{"first", "second"} {
		state.Config.ContextName = contextName
		probeContext(contextName, "")().apply(state)
		cfg := installConfig(state)
		if !slices.Contains(cfg.TLSExtraDNSNames, contextName+".example.test") || !slices.Contains(cfg.TLSExtraDNSNames, "extra.example.test") {
			t.Fatal("current-node or explicit TLS names were lost")
		}
	}
	cfg := installConfig(state)
	if slices.Contains(cfg.TLSExtraDNSNames, "first.example.test") || slices.Contains(cfg.TLSExtraDNSNames, "first.local") {
		t.Fatal("previous cluster widened the new certificate")
	}
	if !slices.Equal(state.Config.TLSExtraDNSNames, []string{"extra.example.test"}) {
		t.Fatal("detected names were stored as operator answers")
	}
	for _, text := range []string{BuildCommandLine(state), stepSummary(state, stepTLS)} {
		if strings.Contains(text, "first.example.test") || !strings.Contains(text, "second.example.test") {
			t.Fatal("review and install use different cluster names")
		}
	}
}

func TestClientIPChoiceCarriesSANsIntoReviewAndSummary(t *testing.T) {
	for _, host := range []string{"192.0.2.10", "2001:db8::10", "127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			state := newWizardState(Options{})
			state.ClusterInfo = clusterInfoMsg{server: "https://api.example.test:6443", nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "first"}}, {ObjectMeta: metav1.ObjectMeta{Name: "second"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: host}}}}}}
			q := questionByID(t, "tls.client")
			if err := q.save(state, []string{host}); err != nil {
				t.Fatal("detected IP not offered", err)
			}
			state.done[stepTLS] = true
			cfg := installConfig(state)
			review := installer.Review(cfg)
			if cfg.ClientHost != host || review.ClientHost != host || !slices.Contains(cfg.TLSExtraIPs, host) || !slices.Contains(review.IPs, host) {
				t.Fatal("chosen IP or SAN lost", review)
			}
			for _, text := range []string{BuildCommandLine(state), stepSummary(state, stepTLS)} {
				if !strings.Contains(text, host) {
					t.Fatal("review/summary omitted IP", text)
				}
			}
			if !strings.Contains(BuildCommandLine(state), "--tls-extra-ip=") {
				t.Fatal("install command missing IP SAN")
			}
			if !strings.Contains(q.explain(state), "NAT isolation") || !strings.Contains(q.explain(state), "port-forward") {
				t.Fatal("routing advice overpromises", q.explain(state))
			}
			// A user can name an address not discovered on any node, such as an LB IP.
			custom := newWizardState(Options{})
			if err := questionByID(t, "tls.names").save(custom, []string{host}); err != nil {
				t.Fatal(err)
			}
			if err := q.save(custom, []string{host}); err != nil {
				t.Fatal("custom IP not offered", err)
			}
			if !slices.Contains(installConfig(custom).TLSExtraIPs, host) {
				t.Fatal("custom IP missing from certificate")
			}
		})
	}
}
