package installer

import (
	"context"
	"errors"
	"net"
)

// ClientDNSProposal describes a public naming remedy; it never edits hosts or DNS.
type ClientDNSProposal struct {
	Host      string `json:"client_host"`
	Address   string `json:"endpoint_address"`
	HostsLine string `json:"hosts_line,omitempty"`
	DNSRecord string `json:"dns_record"`
	Resolves  bool   `json:"resolves_to_endpoint"`
}

func proposeClientDNS(ctx context.Context, host, address string, lookup func(context.Context, string) ([]string, error)) (ClientDNSProposal, error) {
	if address == "" {
		address = host
	}
	address, err := NormalizeClientHost(address)
	if err != nil {
		return ClientDNSProposal{}, errors.New("invalid endpoint address")
	}
	if net.ParseIP(host) != nil || host == address {
		host = "oberth.test"
	}
	result := ClientDNSProposal{Host: host, Address: address}
	if ip := net.ParseIP(address); ip != nil {
		recordType := "A"
		if ip.To4() == nil {
			recordType = "AAAA"
		}
		result.HostsLine = address + " " + host
		result.DNSRecord = host + ". IN " + recordType + " " + address
	} else {
		result.DNSRecord = host + ". IN CNAME " + address + "."
	}
	if lookup == nil {
		return result, nil
	}
	expected := []string{address}
	if net.ParseIP(address) == nil {
		expected, err = lookup(ctx, address)
		if err != nil {
			return result, nil
		}
	}
	actual, err := lookup(ctx, host)
	if err != nil || len(actual) == 0 || len(expected) == 0 {
		return result, nil
	}
	// Every resolved address must belong to this endpoint; a stale extra address
	// can otherwise route a client to a different server.
	for _, candidate := range actual {
		matched := false
		for _, target := range expected {
			if ip := net.ParseIP(candidate); ip != nil && ip.Equal(net.ParseIP(target)) {
				matched = true
			}
		}
		if !matched {
			return result, nil
		}
	}
	result.Resolves = true
	return result, nil
}
