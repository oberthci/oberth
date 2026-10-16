package main

import (
	"slices"
	"testing"
)

func TestServerPKIFromEnvironment(t *testing.T) {
	t.Setenv("OBERTH_SERVER_PKI_MOUNT", "")
	t.Setenv("OBERTH_SERVER_PKI_ROLE", "")
	if _, enabled := serverPKIFromEnvironment("oberth"); enabled {
		t.Fatal("PKI must stay off without a mount and role")
	}
	t.Setenv("OBERTH_SERVER_PKI_MOUNT", "oberth-pki")
	t.Setenv("OBERTH_SERVER_PKI_ROLE", "oberth-server")
	t.Setenv("OBERTH_SERVER_PKI_TTL", "720h")
	t.Setenv("OBERTH_TLS_NAMES", "tuxbox,localhost,127.0.0.1,192.168.86.193")
	pki, enabled := serverPKIFromEnvironment("oberth")
	if !enabled {
		t.Fatal("PKI must be on with a mount and role")
	}
	request := pki.request
	if request.Mount != "oberth-pki" || request.Role != "oberth-server" || request.TTL != "720h" {
		t.Fatalf("request = %+v", request)
	}
	if request.DNSNames[0] != "oberth" || !slices.Contains(request.DNSNames, "tuxbox") || !slices.Contains(request.DNSNames, "oberth-goproxy.oberth.svc") {
		t.Fatalf("dns names = %v", request.DNSNames)
	}
	if !slices.Equal(request.IPs, []string{"127.0.0.1", "192.168.86.193"}) {
		t.Fatalf("ips = %v", request.IPs)
	}
}
