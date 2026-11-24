package installer

import (
	"context"
	"encoding/json"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (e *Engine) networkRanges(ctx context.Context, p *Plan, network *egressNetwork) error {
	policy, _ := p.request.Values["policies"].(map[string]any)
	raw, _ := json.Marshal(policy["clusterCIDRs"])
	var ranges []string
	if json.Unmarshal(raw, &ranges) != nil {
		return networkProblem()
	}
	for _, cidr := range ranges {
		if err := network.exclude(cidr, true); err != nil {
			return err
		}
	}
	nodes, err := e.Cluster.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return networkProblem()
	}
	for _, node := range nodes.Items {
		if err = network.excludeNode(node); err != nil {
			return err
		}
	}
	if err = e.serviceRanges(ctx, network); err != nil {
		return err
	}
	return e.checkAllocatedNetworks(ctx, network)
}

func (n *egressNetwork) excludeNode(node corev1.Node) error {
	cidrs := node.Spec.PodCIDRs
	if len(cidrs) == 0 && node.Spec.PodCIDR != "" {
		cidrs = []string{node.Spec.PodCIDR}
	}
	for _, cidr := range cidrs {
		if err := n.exclude(cidr, true); err != nil {
			return err
		}
	}
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP || address.Type == corev1.NodeExternalIP {
			if err := n.excludeHost(address.Address); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) serviceRanges(ctx context.Context, network *egressNetwork) error {
	serviceRanges, err := e.Cluster.Client.NetworkingV1().ServiceCIDRs().List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
		return networkProblem()
	}
	if err == nil {
		for _, serviceRange := range serviceRanges.Items {
			for _, cidr := range serviceRange.Spec.CIDRs {
				if err = network.exclude(cidr, true); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Hidden public allocation pools need an operator-supplied range. Private pools
// remain covered by the default exclusions even on older Kubernetes versions.
func (e *Engine) checkAllocatedNetworks(ctx context.Context, network *egressNetwork) error {
	services, err := e.Cluster.Client.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return networkProblem()
	}
	for _, service := range services.Items {
		ips := service.Spec.ClusterIPs
		if len(ips) == 0 && service.Spec.ClusterIP != "" {
			ips = []string{service.Spec.ClusterIP}
		}
		for _, ip := range ips {
			if ip != corev1.ClusterIPNone && !network.contains(ip) {
				return networkProblem()
			}
		}
	}
	pods, err := e.Cluster.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return networkProblem()
	}
	for _, pod := range pods.Items {
		for _, ip := range pod.Status.PodIPs {
			if !network.contains(ip.IP) {
				return networkProblem()
			}
		}
	}
	return nil
}

func (n *egressNetwork) contains(raw string) bool {
	address, err := netip.ParseAddr(raw)
	if err != nil {
		return false
	}
	prefixes := []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fe80::/10", "fc00::/7"}
	prefixes = append(prefixes, n.IPv4Except...)
	prefixes = append(prefixes, n.IPv6Except...)
	for _, raw := range prefixes {
		prefix, err := netip.ParsePrefix(raw)
		if err == nil && prefix.Contains(address) {
			n.IPv6 = n.IPv6 || address.Is6()
			return true
		}
	}
	return false
}
