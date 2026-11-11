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
