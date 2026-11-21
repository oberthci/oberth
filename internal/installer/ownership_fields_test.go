package installer_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNamespaceOwnerIncludesReleaseName(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Annotations: map[string]string{
		"meta.helm.sh/release-name": "another", "meta.helm.sh/release-namespace": "oberth",
	}}}
	if _, err := f.client.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	plan, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if plan != nil {
		plan.Close()
	}
	const want = "This namespace belongs to another Helm release.\nChoose another namespace for this installation.\nNamespace: oberth; Helm release: oberth/another"
	if err == nil || err.Error() != want {
		t.Fatal("other release in the same namespace was accepted", err)
	}
	assertNoInstallWrites(t, f)
}

func TestPortOwnershipRequiresInstallationNamespace(t *testing.T) {
	t.Parallel()
	f := setup(t)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "borrowed", Namespace: "elsewhere",
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm"},
		Annotations: map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"},
	}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "https", Port: 443, NodePort: 30443}}}}
	if _, err := f.client.CoreV1().Services("elsewhere").Create(t.Context(), service, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	plan, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if plan != nil {
		plan.Close()
	}
	const want = "HTTPS node port is taken; use --https-node-port.\nChoose an unused node port and review the installation again.\nService: elsewhere/borrowed; node port: 30443"
	if err == nil || err.Error() != want {
		t.Fatal("release marks outside the installation exempted a port holder", err)
	}
	assertNoInstallWrites(t, f)
}
