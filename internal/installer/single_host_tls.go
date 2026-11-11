package installer

import (
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// SingleHostTLSNames uses the selected cluster's node, never the laptop's
// interfaces. Topology, not distribution or API reachability, selects this set.
func SingleHostTLSNames(nodes []corev1.Node, dns, ips []string) ([]string, []string) {
	if len(nodes) != 1 {
		return dns, ips
	}
	dns = unionNames(dns, []string{"localhost"})
	ips = unionNames(ips, []string{"127.0.0.1", "::1"})
	addName := func(raw string) {
		name, err := NormalizeClientHost(raw)
		if err != nil || net.ParseIP(name) != nil {
			return
		}
		dns = unionNames(dns, []string{name})
		short := strings.Split(name, ".")[0]
		dns = unionNames(dns, []string{short + ".local"})
	}
	addName(nodes[0].Name)
	for _, address := range nodes[0].Status.Addresses {
		switch address.Type {
		case corev1.NodeHostName, corev1.NodeInternalDNS, corev1.NodeExternalDNS:
			addName(address.Address)
		case corev1.NodeInternalIP, corev1.NodeExternalIP:
			if ip := net.ParseIP(address.Address); ip != nil && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast() {
				ips = unionNames(ips, []string{ip.String()})
			}
		}
	}
	return dns, ips
}
