package installer_test

import (
	"net/netip"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type networkTarget struct {
	ip, namespace string
	labels        map[string]string
	ports         []corev1.ContainerPort
}

func networkValues(ipv6 bool) map[string]any {
	return map[string]any{"egressNetwork": map[string]any{
		"version": 2, "ipv6": ipv6,
		"apiServers": []any{map[string]any{"cidr": "10.43.0.1/32", "port": 443}, map[string]any{"cidr": "192.168.86.22/32", "port": 6443}},
		"ipv4Except": []any{"198.18.0.0/16", "198.19.0.0/16", "203.0.113.22/32"},
		"ipv6Except": []any{"2001:db8:100::/64", "2001:db8:200::/108", "2001:db8:ffff::22/128"},
	}}
}

func TestDefaultEgressKeepsInternetAndRequiredClusterTraffic(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		objects := rendered(t, networkValues(ipv6))
		var network networkingv1.NetworkPolicy
		decodeObject(t, object(t, objects, "NetworkPolicy", "oberth-egress"), &network)
		checkNetworkPackets(t, network, ipv6)
		checkRenderedServerPorts(t, network, objects)
		var store appsv1.StatefulSet
		decodeObject(t, object(t, objects, "StatefulSet", "oberth-store"), &store)
		if store.Spec.Template.Labels["app.kubernetes.io/name"] != "storeNative" {
			t.Fatal("store selector guard does not use the real subchart label")
		}
		target := networkTarget{ip: "198.18.0.20", namespace: "oberth", labels: store.Spec.Template.Labels}
		if !policyAllows(network, target, corev1.ProtocolTCP, 8200) || policyAllows(network, target, corev1.ProtocolTCP, 8201) {
			t.Fatal("bundled store is unreachable or grants unrelated ports")
		}
	}
}

func checkNetworkPackets(t *testing.T, network networkingv1.NetworkPolicy, ipv6 bool) {
	t.Helper()
	cases := []struct {
		name     string
		target   networkTarget
		protocol corev1.Protocol
		port     int32
		allowed  bool
	}{
		{"internet HTTPS", networkTarget{ip: "93.184.216.34"}, corev1.ProtocolTCP, 443, true},
		{"internet UDP", networkTarget{ip: "93.184.216.34"}, corev1.ProtocolUDP, 443, true},
		{"API Service", networkTarget{ip: "10.43.0.1"}, corev1.ProtocolTCP, 443, true},
		{"API backend", networkTarget{ip: "192.168.86.22"}, corev1.ProtocolTCP, 6443, true},
		{"same namespace", networkTarget{ip: "198.18.0.3", namespace: "oberth-runs"}, corev1.ProtocolTCP, 22, true},
		{"same namespace UDP", networkTarget{ip: "198.18.0.3", namespace: "oberth-runs"}, corev1.ProtocolUDP, 9999, true},
		{"DNS UDP", networkTarget{ip: "198.18.0.53", namespace: "kube-system", labels: map[string]string{"k8s-app": "kube-dns"}}, corev1.ProtocolUDP, 53, true},
		{"DNS TCP", networkTarget{ip: "198.18.0.53", namespace: "kube-system", labels: map[string]string{"k8s-app": "kube-dns"}}, corev1.ProtocolTCP, 53, true},
		{"DNS unrelated port", networkTarget{ip: "198.18.0.53", namespace: "kube-system", labels: map[string]string{"k8s-app": "kube-dns"}}, corev1.ProtocolTCP, 54, false},
		{"metadata", networkTarget{ip: "169.254.169.254"}, corev1.ProtocolTCP, 80, false},
		{"link-local DNS", networkTarget{ip: "169.254.20.10"}, corev1.ProtocolUDP, 53, false},
		{"RFC1918 10", networkTarget{ip: "10.1.2.3"}, corev1.ProtocolTCP, 443, false},
		{"RFC1918 172", networkTarget{ip: "172.16.2.3"}, corev1.ProtocolTCP, 443, false},
		{"RFC1918 192", networkTarget{ip: "192.168.1.3"}, corev1.ProtocolTCP, 443, false},
		{"CGNAT", networkTarget{ip: "100.64.1.3"}, corev1.ProtocolTCP, 443, false},
		{"other namespace", networkTarget{ip: "198.18.0.4", namespace: "other"}, corev1.ProtocolTCP, 22, false},
		{"Service CIDR", networkTarget{ip: "198.19.0.4"}, corev1.ProtocolTCP, 443, false},
		{"private kubelet", networkTarget{ip: "192.168.86.22"}, corev1.ProtocolTCP, 10250, false},
		{"public kubelet", networkTarget{ip: "203.0.113.22"}, corev1.ProtocolTCP, 10250, false},
		{"API other port", networkTarget{ip: "10.43.0.1"}, corev1.ProtocolTCP, 10250, false},
		{"IPv6 internet", networkTarget{ip: "2001:db8:300::1"}, corev1.ProtocolTCP, 443, ipv6},
		{"IPv6 link-local", networkTarget{ip: "fe80::1"}, corev1.ProtocolTCP, 443, false},
		{"IPv6 private", networkTarget{ip: "fd00::1"}, corev1.ProtocolTCP, 443, false},
		{"IPv6 Pod CIDR", networkTarget{ip: "2001:db8:100::4"}, corev1.ProtocolTCP, 22, false},
		{"IPv6 Service CIDR", networkTarget{ip: "2001:db8:200::4"}, corev1.ProtocolTCP, 443, false},
		{"IPv6 kubelet", networkTarget{ip: "2001:db8:ffff::22"}, corev1.ProtocolTCP, 10250, false},
	}
	for _, test := range cases {
		if policyAllows(network, test.target, test.protocol, test.port) != test.allowed {
			t.Fatal("egress reachability differs:", test.name, "want allowed", test.allowed)
		}
	}
}

func checkRenderedServerPorts(t *testing.T, network networkingv1.NetworkPolicy, objects []map[string]any) {
	t.Helper()
	pod := serverDeployment(t, objects).Spec.Template
	target := networkTarget{ip: "198.18.0.10", namespace: "oberth", labels: pod.Labels, ports: pod.Spec.Containers[0].Ports}
	ssh, count := false, 0
	for _, name := range []string{"oberth", "oberth-source"} {
		var service corev1.Service
		decodeObject(t, object(t, objects, "Service", name), &service)
		for _, port := range service.Spec.Ports {
			actual := serviceTargetPort(port, target.ports)
			if actual == 0 || !policyAllows(network, target, corev1.ProtocolTCP, actual) {
				t.Fatal("server Service port is not admitted", name, port.Name, actual)
			}
			ssh = ssh || (port.Name == "ssh" && port.Port == 2222)
			count++
		}
	}
	if !ssh || count != 3 || policyAllows(network, target, corev1.ProtocolTCP, 8081) {
		t.Fatal("server ports do not match required Services")
	}
}

func serviceTargetPort(port corev1.ServicePort, ports []corev1.ContainerPort) int32 {
	if port.TargetPort.Type == intstr.Int {
		return port.TargetPort.IntVal
	}
	for _, containerPort := range ports {
		if containerPort.Name == port.TargetPort.StrVal {
			return containerPort.ContainerPort
		}
	}
	return 0
}

func policyAllows(network networkingv1.NetworkPolicy, target networkTarget, protocol corev1.Protocol, port int32) bool {
	for _, rule := range network.Spec.Egress {
		if !rulePortMatches(rule.Ports, target, protocol, port) {
			continue
		}
		if len(rule.To) == 0 {
			return true
		}
		for _, peer := range rule.To {
			if peerMatches(network.Namespace, peer, target) {
				return true
			}
		}
	}
	return false
}

func rulePortMatches(ports []networkingv1.NetworkPolicyPort, target networkTarget, protocol corev1.Protocol, port int32) bool {
	if len(ports) == 0 {
		return true
	}
	for _, allowed := range ports {
		p := corev1.ProtocolTCP
		if allowed.Protocol != nil {
			p = *allowed.Protocol
		}
		if p != protocol {
			continue
		}
		if allowed.Port == nil {
			return true
		}
		if allowed.Port.Type == intstr.Int && allowed.Port.IntVal == port {
			return true
		}
		for _, containerPort := range target.ports {
			if allowed.Port.Type == intstr.String && containerPort.Name == allowed.Port.StrVal && containerPort.ContainerPort == port {
				return true
			}
		}
	}
	return false
}

func peerMatches(namespace string, peer networkingv1.NetworkPolicyPeer, target networkTarget) bool {
	if peer.IPBlock != nil {
		address, err := netip.ParseAddr(target.ip)
		prefix, prefixErr := netip.ParsePrefix(peer.IPBlock.CIDR)
		if err != nil || prefixErr != nil || !prefix.Contains(address) {
			return false
		}
		for _, cidr := range peer.IPBlock.Except {
			if netip.MustParsePrefix(cidr).Contains(address) {
				return false
			}
		}
		return true
	}
	if target.namespace == "" {
		return false
	}
	if peer.NamespaceSelector == nil && namespace != target.namespace {
		return false
	}
	return selectorMatches(peer.NamespaceSelector, map[string]string{"kubernetes.io/metadata.name": target.namespace}) && selectorMatches(peer.PodSelector, target.labels)
}

func selectorMatches(selector *metav1.LabelSelector, values map[string]string) bool {
	if selector == nil {
		return true
	}
	parsed, err := metav1.LabelSelectorAsSelector(selector)
	return err == nil && parsed.Matches(labels.Set(values))
}

func TestExplicitEgressCanNarrowInternetAndAllowInternalUDP(t *testing.T) {
	values := networkValues(false)
	values["policies"] = map[string]any{"internet": false, "allow": []any{
		map[string]any{"cidr": "93.184.216.0/24", "port": 443, "protocol": "TCP"},
		map[string]any{"cidr": "169.254.20.10/32", "port": 53, "protocol": "UDP"},
	}}
	var network networkingv1.NetworkPolicy
	decodeObject(t, object(t, rendered(t, values), "NetworkPolicy", "oberth-egress"), &network)
	if !policyAllows(network, networkTarget{ip: "93.184.216.34"}, corev1.ProtocolTCP, 443) || policyAllows(network, networkTarget{ip: "93.184.216.34"}, corev1.ProtocolTCP, 80) || policyAllows(network, networkTarget{ip: "93.184.216.34"}, corev1.ProtocolUDP, 443) {
		t.Fatal("explicit internet destinations were widened")
	}
	if !policyAllows(network, networkTarget{ip: "169.254.20.10"}, corev1.ProtocolUDP, 53) || policyAllows(network, networkTarget{ip: "169.254.20.10"}, corev1.ProtocolTCP, 53) {
		t.Fatal("explicit internal UDP exception lost its protocol")
	}
	if !policyAllows(network, networkTarget{ip: "10.43.0.1"}, corev1.ProtocolTCP, 443) {
		t.Fatal("narrowing internet broke the API")
	}
	values["policies"].(map[string]any)["egress"] = false
	for _, obj := range rendered(t, values) {
		if obj["kind"] == "NetworkPolicy" && obj["metadata"].(map[string]any)["name"] == "oberth-egress" {
			t.Fatal("opting out retained the egress policy")
		}
	}
}
