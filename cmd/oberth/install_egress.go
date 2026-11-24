package main

import (
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
)

type egressDestinations struct{ entries []any }

type egressDestination struct {
	CIDR     string `json:"cidr"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

func (e egressDestination) valid() bool {
	prefix, err := netip.ParsePrefix(e.CIDR)
	return err == nil && prefix == prefix.Masked() && e.Port >= 1 && e.Port <= 65535 && (e.Protocol == "TCP" || e.Protocol == "UDP")
}

func (v *egressDestinations) String() string {
	if v == nil || v.entries == nil {
		return "[]"
	}
	raw, _ := json.Marshal(v.entries)
	return string(raw)
}

func (v *egressDestinations) Set(raw string) error {
	var entries []egressDestination
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 32<<10 || !json.Valid([]byte(raw)) || decoder.Decode(&entries) != nil || entries == nil || len(entries) > 128 {
		return errors.New("use a JSON array of CIDR, port and TCP/UDP protocol entries")
	}
	next := make([]any, 0, len(entries))
	for _, entry := range entries {
		if !entry.valid() {
			return errors.New("use canonical CIDRs, ports from 1 to 65535 and TCP or UDP")
		}
		next = append(next, map[string]any{"cidr": entry.CIDR, "port": entry.Port, "protocol": entry.Protocol})
	}
	v.entries = next
	return nil
}

type clusterCIDRs struct{ entries []string }

func (v *clusterCIDRs) String() string {
	if v == nil || v.entries == nil {
		return "[]"
	}
	raw, _ := json.Marshal(v.entries)
	return string(raw)
}
func (v *clusterCIDRs) Set(raw string) error {
	var entries []string
	if json.Unmarshal([]byte(raw), &entries) != nil || entries == nil || len(entries) > 128 {
		return errors.New("use a JSON array of cluster Pod and Service CIDRs")
	}
	for _, value := range entries {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 {
			return errors.New("use canonical cluster Pod and Service CIDRs")
		}
	}
	v.entries = entries
	return nil
}
