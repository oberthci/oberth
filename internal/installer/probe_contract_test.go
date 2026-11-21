package installer_test

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestProbePortIsNodeOnlyAndNeverAService(t *testing.T) {
	t.Parallel()
	tcp := corev1.ProtocolTCP
	want := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "oberth"}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(8443))},
			{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(2222))},
			{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(8445))},
		}}},
	}
	for _, egress := range []bool{true, false} {
		objects := rendered(t, map[string]any{"policies": map[string]any{"egress": egress}})
		var policy networkingv1.NetworkPolicy
		decodeObject(t, object(t, objects, "NetworkPolicy", "oberth-ingress"), &policy)
		if policy.Namespace != "oberth" || !reflect.DeepEqual(policy.Spec, want) {
			t.Fatal("server ingress must leave only the local-node allowance for probes", policy)
		}
		assertNoProbeService(t, objects)
	}
}

func assertNoProbeService(t *testing.T, objects []map[string]any) {
	t.Helper()
	for _, item := range objects {
		if item["kind"] != "Service" {
			continue
		}
		var service corev1.Service
		decodeObject(t, item, &service)
		for _, port := range service.Spec.Ports {
			if port.Port == 8081 || port.TargetPort.IntVal == 8081 || port.TargetPort.StrVal == "health" {
				t.Fatal("probe exposed by Service", service.Name, port)
			}
		}
	}
}
