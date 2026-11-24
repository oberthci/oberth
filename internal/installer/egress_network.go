package installer

import (
	"context"
	"encoding/json"
	"net/netip"
	"reflect"
	"slices"

	"github.com/oberthci/oberth/internal/publicerr"
)

type networkDestination struct {
	CIDR string `json:"cidr"`
	Port int32  `json:"port"`
}

type egressNetwork struct {
	Version    int                  `json:"version"`
	IPv6       bool                 `json:"ipv6"`
	APIServers []networkDestination `json:"apiServers"`
	IPv4Except []string             `json:"ipv4Except"`
	IPv6Except []string             `json:"ipv6Except"`
}

func (e *Engine) networkSnapshot(ctx context.Context, p *Plan) (map[string]any, error) {
	network := egressNetwork{Version: 2, APIServers: []networkDestination{}, IPv4Except: []string{}, IPv6Except: []string{}}
	if settingText(p.request.Values, "policies", "egress") == "on" {
		if err := e.networkRanges(ctx, p, &network); err != nil {
			return nil, err
		}
		if err := e.apiDestinations(ctx, &network); err != nil {
			return nil, err
		}
	}
	network.normalize()
	raw, _ := json.Marshal(network)
	var values map[string]any
	_ = json.Unmarshal(raw, &values)
	return values, nil
}

func (n *egressNetwork) exclude(raw string, network bool) error {
	prefix, err := netip.ParsePrefix(raw)
	if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 {
		return networkProblem()
	}
	if prefix.Addr().Is4() {
		n.IPv4Except = append(n.IPv4Except, prefix.String())
	} else {
		n.IPv6Except = append(n.IPv6Except, prefix.String())
		n.IPv6 = n.IPv6 || network
	}
	return nil
}

func (n *egressNetwork) excludeHost(raw string) error {
	address, err := netip.ParseAddr(raw)
	if err != nil {
		return networkProblem()
	}
	return n.exclude(netip.PrefixFrom(address, address.BitLen()).String(), false)
}

func (n *egressNetwork) normalize() {
	slices.Sort(n.IPv4Except)
	n.IPv4Except = slices.Compact(n.IPv4Except)
	slices.Sort(n.IPv6Except)
	n.IPv6Except = slices.Compact(n.IPv6Except)
	slices.SortFunc(n.APIServers, func(a, b networkDestination) int {
		if a.CIDR < b.CIDR {
			return -1
		}
		if a.CIDR > b.CIDR {
			return 1
		}
		return int(a.Port - b.Port)
	})
	n.APIServers = slices.Compact(n.APIServers)
}

func (e *Engine) recheckNetwork(ctx context.Context, p *Plan) error {
	if _, present := p.request.Values["egressNetwork"]; !present {
		return nil
	}
	next, err := e.networkSnapshot(ctx, p)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(next, p.request.Values["egressNetwork"]) {
		return described("target", nil, "Cluster network changed after review.")
	}
	return nil
}

func networkProblem() error {
	return publicerr.New(publicerr.InvalidInput, "Cannot review the cluster network.", "Allow network inventory reads; supply hidden Pod/Service ranges with --cluster-cidrs, then review again.", nil)
}
