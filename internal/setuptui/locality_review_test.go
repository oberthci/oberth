package setuptui

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestLocalServerUsesParsedHostAndExactPrivateRanges(t *testing.T) {
	for _, host := range []string{"https://localhost.evil.example", "https://172.2.1.1", "https://172.32.1.1", "https://172.39.1.1"} {
		if isLocalServer(host) {
			t.Errorf("remote host classified local: %s", host)
		}
	}
	for _, host := range []string{"https://localhost:6443", "https://172.20.1.1", "https://[::1]:6443"} {
		if !isLocalServer(host) {
			t.Errorf("local host classified remote: %s", host)
		}
	}
}

func TestInstallConfigRequiresExplicitRemoteAcknowledgement(t *testing.T) {
	state := &WizardState{}
	state.ClusterInfo = clusterInfoMsg{context: "remote", server: "https://remote.example", isLocal: false}
	if installConfig(state).Yes {
		t.Fatal("remote consent invented before review acknowledgement")
	}
	state.remoteAcknowledged = true
	if !installConfig(state).Yes {
		t.Fatal("explicit remote acknowledgement not passed to installer")
	}
}

func TestRemoteReviewRequiresSeparateAcknowledgement(t *testing.T) {
	newTestHost(t)
	fake := stubInstaller(t, func(_ int, _ installer.InstallDeps) error { return nil })
	h := newHarness(t, Options{})
	h.answerAllSteps()
	h.w.state.ClusterInfo = clusterInfoMsg{context: "remote", server: "https://localhost.evil.example", isLocal: false}
	h.enter()
	if fake.calls != 0 || h.w.state.remoteAcknowledged {
		t.Fatal("first continue authorized remote installation")
	}
	if !strings.Contains(h.view(), "Confirm remote install") || !strings.Contains(h.view(), "localhost.evil.example") {
		t.Fatalf("missing target acknowledgement: %s", h.view())
	}
	h.enter()
	if fake.calls != 1 || !fake.cfgs[0].Yes {
		t.Fatalf("explicit acknowledgement did not reach installer: %d", fake.calls)
	}
}
