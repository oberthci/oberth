package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPluginDNSRemedyWaitsForNameAndReprobes(t *testing.T) {
	for _, address := range []string{"192.0.2.10", "2001:db8::10", "edge.example.test"} {
		t.Run(address, func(t *testing.T) {
			configured := false
			deps := ClientPortProbeDeps{EndpointAddress: address, LookupHost: func(_ context.Context, host string) ([]string, error) {
				if host == address || configured {
					return []string{"192.0.2.10", "2001:db8::10"}, nil
				}
				return nil, errors.New("not configured")
			}}
			host := "oberth.test"
			if address != "edge.example.test" {
				host = address
			}
			result, err := ProbeClientPorts(t.Context(), probePlan(), Config{ClientHost: host}, deps)
			if err != nil || result.DNS == nil || result.DNS.Resolves || !result.NeedsAction || result.Host != "oberth.test" {
				t.Fatalf("%+v %v", result, err)
			}
			if !strings.Contains(result.DNS.DNSRecord, address) || !strings.Contains(result.Advice, "wait for the user") {
				t.Fatal(result)
			}
			if address != "edge.example.test" && result.DNS.HostsLine != address+" oberth.test" {
				t.Fatal(result.DNS)
			}
			configured = true
			deps.LookupHost = func(_ context.Context, name string) ([]string, error) {
				if address == "edge.example.test" {
					return []string{"192.0.2.10"}, nil
				}
				return []string{address}, nil
			}
			result, err = ProbeClientPorts(t.Context(), probePlan(), Config{ClientHost: result.Host}, deps)
			if err != nil || !result.DNS.Resolves {
				t.Fatalf("name not re-probed: %+v %v", result, err)
			}
		})
	}
}

func TestPluginDNSRejectsStaleOrInvalidAddresses(t *testing.T) {
	deps := ClientPortProbeDeps{EndpointAddress: "192.0.2.10", LookupHost: func(context.Context, string) ([]string, error) { return []string{"192.0.2.10", "192.0.2.11"}, nil }}
	result, err := ProbeClientPorts(t.Context(), probePlan(), Config{ClientHost: "chosen.example.test"}, deps)
	if err != nil || result.DNS.Resolves {
		t.Fatalf("%+v %v", result, err)
	}
	deps.EndpointAddress = "host;command"
	if _, err := ProbeClientPorts(t.Context(), probePlan(), Config{}, deps); err == nil {
		t.Fatal("accepted invalid endpoint")
	}
}
