package installer

import (
	"strings"
	"testing"
)

func TestProductInstallDoesNotConfigureAnInstanceTunnel(t *testing.T) {
	args := OberthHelmArgs(Config{Namespace: "ci", ChartPath: "chart", ClientHost: "ci.example.test"}, OpenBaoResult{ServerCACertPEM: "public-server-ca"}, RekorResult{})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "watchTunnel") || strings.Contains(joined, "cloudflared") {
		t.Fatalf("product installer still owns an instance connector: %s", joined)
	}
	if !strings.Contains(joined, "pushBannerURL=https://ci.example.test:") {
		t.Fatalf("generic dashboard link was lost: %s", joined)
	}
}

func TestProductUpgradeDoesNotConfigureAnInstanceTunnel(t *testing.T) {
	_, args := upgradeHelmArgs(UpgradeConfig{
		Namespace: "ci", serverPKI: true,
		TLSExtraDNSNames: []string{"ci.example.test"}, TLSExtraIPs: []string{"192.0.2.1"},
	}, "chart", "example.invalid/oberth@sha256:abc", backupCompatibility{})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "watchTunnel") || strings.Contains(joined, "cloudflared") {
		t.Fatalf("product upgrade still configures an instance connector: %s", joined)
	}
	for _, required := range []string{`tls.extraDNSNames=["ci.example.test"]`, `tls.extraIPs=["192.0.2.1"]`} {
		if !strings.Contains(joined, required) {
			t.Fatalf("server PKI setting lost: %s", required)
		}
	}
}
