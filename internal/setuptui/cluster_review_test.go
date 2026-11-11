package setuptui

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

// Any cluster the kubeconfig reaches is a valid target: the review names the
// context and its API server, and the one Continue on it installs. There is no
// second, locality-based confirmation and no --yes in the equivalent command.
func TestReviewInstallsOnAnyClusterAfterOneConfirmation(t *testing.T) {
	newTestHost(t)
	fake := stubInstaller(t, func(_ int, _ installer.InstallDeps) error { return nil })
	h := newHarness(t, Options{})
	h.answerAllSteps()
	h.w.state.ClusterInfo = clusterInfoMsg{context: "prod", server: "https://cluster.example.test:6443"}
	h.w.state.Config.ContextName = "prod"
	if !strings.Contains(h.view(), "cluster.example.test") {
		t.Fatalf("review does not name the API server: %s", h.view())
	}
	h.enter()
	if fake.calls != 1 {
		t.Fatalf("review Continue did not install: %d calls", fake.calls)
	}
	if strings.Contains(BuildCommandLine(&h.w.state), "--yes") {
		t.Fatal("the equivalent command carries --yes")
	}
}

func TestContextOptionsNameServerWithoutLocality(t *testing.T) {
	opts := contextOptions([]kubeContext{{name: "a", server: "https://127.0.0.1:6443"}, {name: "b", server: "https://cluster.example.test"}}, "a")
	if opts[0].detail != "current · https://127.0.0.1:6443" || opts[1].detail != "https://cluster.example.test" {
		t.Fatalf("details = %q, %q", opts[0].detail, opts[1].detail)
	}
	for _, o := range opts {
		if strings.Contains(o.detail, "local") || strings.Contains(o.detail, "remote") {
			t.Fatalf("context labelled by locality: %q", o.detail)
		}
	}
}
