package installer

import (
	"context"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (e *Engine) apiDestinations(ctx context.Context, network *egressNetwork) error {
	service, err := e.Cluster.Client.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return networkProblem()
	}
	if err = network.apiService(service); err != nil {
		return err
	}
	slices, err := e.Cluster.Client.DiscoveryV1().EndpointSlices("default").List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=kubernetes"})
	if err != nil {
		return networkProblem()
	}
	count := 0
	for _, slice := range slices.Items {
		added, err := network.apiSlice(slice)
		if err != nil {
			return err
		}
		count += added
	}
	if count == 0 {
		return e.coreAPIEndpoints(ctx, network)
	}
	return nil
}

func (n *egressNetwork) apiService(service *corev1.Service) error {
	ips := service.Spec.ClusterIPs
	if len(ips) == 0 {
		ips = []string{service.Spec.ClusterIP}
	}
	for _, ip := range ips {
		for _, port := range service.Spec.Ports {
			if port.Protocol == "" || port.Protocol == corev1.ProtocolTCP {
				if err := n.api(ip, port.Port); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (n *egressNetwork) apiSlice(slice discoveryv1.EndpointSlice) (int, error) {
	var ports []int32
	for _, port := range slice.Ports {
		if port.Port != nil && (port.Protocol == nil || *port.Protocol == corev1.ProtocolTCP) {
			ports = append(ports, *port.Port)
		}
	}
	count := 0
	for _, endpoint := range slice.Endpoints {
		for _, address := range endpoint.Addresses {
			for _, port := range ports {
				if err := n.api(address, port); err != nil {
					return 0, err
				}
				count++
			}
		}
	}
	return count, nil
}

// Older clusters may expose the built-in API only through the core Endpoints API.
func (e *Engine) coreAPIEndpoints(ctx context.Context, network *egressNetwork) error {
	endpoints, err := e.Cluster.Client.CoreV1().Endpoints("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return networkProblem()
	}
	count := 0
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			for _, port := range subset.Ports {
				if port.Protocol == "" || port.Protocol == corev1.ProtocolTCP {
					if err = network.api(address.IP, port.Port); err != nil {
						return err
					}
					count++
				}
			}
		}
	}
	if count == 0 {
		return networkProblem()
	}
	return nil
}

func (n *egressNetwork) api(raw string, port int32) error {
	address, err := netip.ParseAddr(raw)
	if err != nil || port < 1 || port > 65535 {
		return networkProblem()
	}
	prefix := netip.PrefixFrom(address, address.BitLen()).String()
	n.APIServers = append(n.APIServers, networkDestination{CIDR: prefix, Port: port})
	return n.exclude(prefix, address.Is6())
}
