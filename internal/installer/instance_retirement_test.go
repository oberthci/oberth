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
