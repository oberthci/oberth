package integration_test

import (
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// TestChartServiceMigrationUsesAnAdoptionRevision models Helm's client-side
// three-way patch path with Kubernetes' real Service patch metadata. Service
// ports merge by port (not by name), so a live SSH port that drifted from the
// last Helm manifest cannot be reconciled by rendering that old manifest
// again. An explicit adoption revision first makes 2222 Helm-owned; the next
// normal revision can then move that merge key to 22 without force/replacement.
func TestChartServiceMigrationUsesAnAdoptionRevision(t *testing.T) {
	t.Parallel()

	lastApplied := migrationService(corev1.ServiceTypeNodePort, 22)
	live := migrationService(corev1.ServiceTypeLoadBalancer, 2222)
	desired := migrationService(corev1.ServiceTypeNodePort, 22)

	direct := helmThreeWayServicePatch(t, lastApplied, desired, live)
	directSSHPorts := make(map[int32]int)
	for _, port := range direct.Spec.Ports {
		if port.Name == "ssh" && port.NodePort == 30022 {
			directSSHPorts[port.Port]++
		}
	}
	if !reflect.DeepEqual(directSSHPorts, map[int32]int{22: 1, 2222: 1}) {
		t.Fatalf("direct patch SSH ports = %v, want duplicate logical SSH port under merge keys 22 and 2222", directSSHPorts)
	}

	adoption := migrationService(corev1.ServiceTypeLoadBalancer, 2222)
	adopted := helmThreeWayServicePatch(t, lastApplied, adoption, live)
	if !reflect.DeepEqual(adopted.Spec, adoption.Spec) {
		t.Fatalf("adoption revision = %#v, want exact live Service %#v", adopted.Spec, adoption.Spec)
	}

	migrated := helmThreeWayServicePatch(t, adoption, desired, adopted)
	if !reflect.DeepEqual(migrated.Spec, desired.Spec) {
		t.Fatalf("migration revision = %#v, want %#v", migrated.Spec, desired.Spec)
	}
}

func migrationService(serviceType corev1.ServiceType, sshPort int32) corev1.Service {
	return corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oberth",
			Namespace: "oberth",
		},
		Spec: corev1.ServiceSpec{
			Type: serviceType,
			Selector: map[string]string{
				"app.kubernetes.io/name": "oberth",
			},
			Ports: []corev1.ServicePort{
				{Name: "ssh", Protocol: corev1.ProtocolTCP, Port: sshPort, TargetPort: intstr.FromString("ssh"), NodePort: 30022},
				{Name: "https", Protocol: corev1.ProtocolTCP, Port: 443, TargetPort: intstr.FromString("https"), NodePort: 30443},
			},
		},
	}
}

func helmThreeWayServicePatch(t *testing.T, original, modified, current corev1.Service) corev1.Service {
	t.Helper()
	encode := func(service corev1.Service) []byte {
		body, err := json.Marshal(service)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	metadata, err := strategicpatch.NewPatchMetaFromStruct(corev1.Service{})
	if err != nil {
		t.Fatal(err)
	}
	patch, err := strategicpatch.CreateThreeWayMergePatch(
		encode(original), encode(modified), encode(current), metadata, true,
	)
	if err != nil {
		t.Fatalf("create Helm-style three-way patch: %v", err)
	}
	result, err := strategicpatch.StrategicMergePatch(encode(current), patch, corev1.Service{})
	if err != nil {
		t.Fatalf("apply Helm-style strategic merge patch %s: %v", patch, err)
	}
	var service corev1.Service
	if err := json.Unmarshal(result, &service); err != nil {
		t.Fatal(err)
	}
	return service
}
