package installer

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/secretstore"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestOpenBaoTLSBootstrapPodUsesPrecreatedPVCWithoutCredentials(t *testing.T) {
	t.Parallel()
	const image = "registry.example/oberth@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pod := openBaoTLSBootstrapPod("openbao", image, "node-a")

	if pod.Namespace != "openbao" || pod.Name != openBaoTLSBootstrapPodName || pod.Spec.NodeName != "node-a" {
		t.Fatalf("bootstrap pod identity = %s/%s node=%q", pod.Namespace, pod.Name, pod.Spec.NodeName)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("bootstrap pod must not receive a ServiceAccount token")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || len(pod.Spec.Containers) != 1 {
		t.Fatalf("bootstrap pod restart=%q containers=%d", pod.Spec.RestartPolicy, len(pod.Spec.Containers))
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsUser == nil ||
		*pod.Spec.SecurityContext.RunAsUser != 65534 || pod.Spec.SecurityContext.RunAsGroup == nil ||
		*pod.Spec.SecurityContext.RunAsGroup != 1000 || pod.Spec.SecurityContext.FSGroup == nil ||
		*pod.Spec.SecurityContext.FSGroup != 1000 {
		t.Fatalf("bootstrap pod identity context = %+v", pod.Spec.SecurityContext)
	}
	if pod.Spec.SecurityContext.FSGroupChangePolicy == nil || *pod.Spec.SecurityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Fatalf("bootstrap pod must preserve fixed TLS file modes on repeat mounts: %+v", pod.Spec.SecurityContext)
	}
	container := pod.Spec.Containers[0]
	if container.Image != image || len(container.Env) != 0 || len(container.EnvFrom) != 0 {
		t.Fatalf("bootstrap container image=%q env=%d envFrom=%d", container.Image, len(container.Env), len(container.EnvFrom))
	}
	if len(container.Args) < 2 || container.Args[0] != "secretstore" || container.Args[1] != "bootstrap-tls" {
		t.Fatalf("bootstrap args = %q", container.Args)
	}
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*container.SecurityContext.ReadOnlyRootFilesystem || container.SecurityContext.AllowPrivilegeEscalation == nil ||
		*container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("bootstrap security context = %+v", container.SecurityContext)
	}
	if container.SecurityContext.Capabilities == nil || len(container.SecurityContext.Capabilities.Drop) != 1 ||
		container.SecurityContext.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Fatalf("bootstrap capabilities = %+v", container.SecurityContext.Capabilities)
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].PersistentVolumeClaim == nil ||
		pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != openBaoTLSDataClaimName {
		t.Fatalf("bootstrap volumes = %+v", pod.Spec.Volumes)
	}
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].MountPath != openBaoDataMountPath ||
		container.VolumeMounts[0].ReadOnly {
		t.Fatalf("bootstrap mounts = %+v", container.VolumeMounts)
	}
}

// TestOpenBaoDataPVCSelectsTheNodeTheBootstrapPodIsPinnedTo covers the deadlock
// that made --install-secretstore unusable on k3s, which is the platform Oberth
// documents as its default.
//
// The bootstrap Pod sets spec.nodeName so the volume lands on the node the
// OpenBao StatefulSet already occupies. Setting nodeName bypasses the
// scheduler, and the scheduler is what binds a claim whose StorageClass uses
// volumeBindingMode: WaitForFirstConsumer -- k3s local-path, and the default on
// most managed clusters. Measured on a real cluster: the Pod sat in
// ContainerCreating with "PVC is not bound" while the claim reported "waiting
// for first consumer to be created before binding", each waiting for the other
// until the install timed out.
func TestOpenBaoDataPVCSelectsTheNodeTheBootstrapPodIsPinnedTo(t *testing.T) {
	t.Parallel()
	pinned := openBaoDataPVC("openbao", "tuxbox")
	if got := pinned.Annotations[selectedNodeAnnotation]; got != "tuxbox" {
		t.Fatalf("claim %s = %q, want the pinned node; a WaitForFirstConsumer class never provisions it",
			selectedNodeAnnotation, got)
	}
	// With no existing OpenBao pod there is no node to pin to, the bootstrap
	// Pod carries no nodeName, and the scheduler makes the decision itself.
	// Pre-empting it here would place the volume before anything knows where
	// the workload fits.
	unpinned := openBaoDataPVC("openbao", "")
	if _, present := unpinned.Annotations[selectedNodeAnnotation]; present {
		t.Fatalf("claim selects a node when the bootstrap Pod is unpinned: %v", unpinned.Annotations)
	}
}

func TestOpenBaoDataPVCMatchesOfficialStatefulSetClaim(t *testing.T) {
	t.Parallel()
	claim := openBaoDataPVC("openbao", "")
	if claim.Namespace != "openbao" || claim.Name != "data-openbao-0" {
		t.Fatalf("claim identity = %s/%s", claim.Namespace, claim.Name)
	}
	if len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("claim access modes = %v", claim.Spec.AccessModes)
	}
	want := "10Gi"
	if got := claim.Spec.Resources.Requests.Storage().String(); got != want {
		t.Fatalf("claim storage = %s, want %s", got, want)
	}
	if claim.Spec.StorageClassName != nil {
		t.Fatalf("claim must use the cluster default storage class, got %q", *claim.Spec.StorageClassName)
	}
}

func TestValidateOpenBaoTLSBootstrapPodRejectsSecurityDrift(t *testing.T) {
	t.Parallel()
	const image = "registry.example/oberth@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	want := openBaoTLSBootstrapPod("openbao", image, "node-a")
	tests := map[string]func(*corev1.Pod){
		"environment": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "TOKEN", Value: "unexpected"}}
		},
		"privileged": func(pod *corev1.Pod) {
			value := true
			pod.Spec.Containers[0].SecurityContext.Privileged = &value
		},
		"wrong PVC": func(pod *corev1.Pod) {
			pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "attacker-data"
		},
		"writable root": func(pod *corev1.Pod) {
			value := false
			pod.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem = &value
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := want.DeepCopy()
			mutate(got)
			if err := validateOpenBaoTLSBootstrapPod(got, want); err == nil {
				t.Fatal("drifted bootstrap pod must be rejected")
			}
		})
	}
}

func TestEnsureOpenBaoTLSOrdersPVCPodAndPublicCAExtraction(t *testing.T) {
	t.Parallel()
	defaultSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "openbao"}}
	client := fake.NewClientset(defaultSA)
	var actions []string
	client.PrependReactor("create", "namespaces", func(action ktesting.Action) (bool, runtime.Object, error) {
		create := action.(ktesting.CreateAction)
		namespace := create.GetObject().(*corev1.Namespace)
		actions = append(actions, "namespace:"+namespace.Name)
		return false, nil, nil
	})
	client.PrependReactor("create", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		create := action.(ktesting.CreateAction)
		claim := create.GetObject().(*corev1.PersistentVolumeClaim)
		actions = append(actions, "pvc:"+claim.Name)
		return false, nil, nil
	})
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		create := action.(ktesting.CreateAction)
		pod := create.GetObject().(*corev1.Pod).DeepCopy()
		actions = append(actions, "pod:"+pod.Name)
		pod.Status.Phase = corev1.PodSucceeded
		return true, pod, nil
	})
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, "bootstrap-deleted")
		return false, nil, nil
	})
	caPEMBytes, err := secretstore.BootstrapOpenBaoTLS(filepath.Join(t.TempDir(), "tls"), "openbao")
	if err != nil {
		t.Fatal(err)
	}
	caPEM := string(caPEMBytes)
	deps := Deps{
		Output:     io.Discard,
		KubeClient: client,
		PodLogs: func(_ context.Context, namespace, pod string) ([]byte, error) {
			actions = append(actions, "logs:"+namespace+"/"+pod)
			return []byte(caPEM), nil
		},
	}
	got, err := ensureOpenBaoTLS(context.Background(), Config{OpenBaoNamespace: "openbao"}, deps,
		"registry.example/oberth@sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if got != caPEM {
		t.Fatalf("CA = %q", got)
	}
	want := []string{"namespace:openbao", "pvc:data-openbao-0", "pod:openbao-tls-bootstrap", "logs:openbao/openbao-tls-bootstrap", "bootstrap-deleted"}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("openbao").Get(context.Background(), openBaoTLSDataClaimName, metav1.GetOptions{}); err != nil {
		t.Fatalf("precreated claim was not retained: %v", err)
	}
}

func TestInstallOpenBaoProductionBootstrapsTLSBeforeHelm(t *testing.T) {
	t.Parallel()
	defaultSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "openbao"}}
	client := fake.NewClientset(defaultSA)
	var actions []string
	client.PrependReactor("create", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		claim := action.(ktesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
		actions = append(actions, "pvc:"+claim.Name)
		return false, nil, nil
	})
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		actions = append(actions, "pod:"+pod.Name)
		pod.Status.Phase = corev1.PodSucceeded
		return true, pod, nil
	})
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, "bootstrap-deleted")
		return false, nil, nil
	})
	caPEM, err := secretstore.BootstrapOpenBaoTLS(filepath.Join(t.TempDir(), "tls"), "openbao")
	if err != nil {
		t.Fatal(err)
	}
	digestImage := "registry.example/oberth@sha256:" + strings.Repeat("b", 64)
	deps := Deps{
		Output:     io.Discard,
		KubeClient: client,
		PodLogs: func(_ context.Context, _, _ string) ([]byte, error) {
			actions = append(actions, "bootstrap-logs")
			return caPEM, nil
		},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch {
			case len(args) >= 3 && args[0] == "repo" && args[2] == openBaoRepoName:
				actions = append(actions, "openbao-repo")
			case len(args) >= 1 && args[0] == "list":
				actions = append(actions, "release-check")
				return []byte("[]"), nil
			case len(args) >= 3 && args[0] == "repo" && args[2] == oberthRepoName:
				actions = append(actions, "oberth-repo")
			case len(args) >= 2 && args[0] == "show":
				actions = append(actions, "image-resolve")
				return []byte("image:\n  ref: " + digestImage + "\n"), nil
			case len(args) >= 1 && args[0] == "upgrade":
				actions = append(actions, "openbao-helm")
			}
			return nil, nil
		},
	}
	result, err := InstallOpenBao(context.Background(), Config{
		InstallSecretStore:  true,
		OpenBaoNamespace:    "openbao",
		OpenBaoChartVersion: DefaultOpenBaoChartVersion,
		Timeout:             time.Second,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.ServiceAddress != "https://openbao.openbao.svc:8200" || result.CACertPEM != string(caPEM) {
		t.Fatalf("secure OpenBao result = %+v", result)
	}
	bootstrapLogs := slices.Index(actions, "bootstrap-logs")
	bootstrapDeleted := slices.Index(actions, "bootstrap-deleted")
	helmInstall := slices.Index(actions, "openbao-helm")
	if bootstrapLogs < 0 || bootstrapDeleted < 0 || helmInstall < 0 || bootstrapLogs >= bootstrapDeleted || bootstrapDeleted >= helmInstall {
		t.Fatalf("TLS material must exist before Helm creates the listener, actions = %v", actions)
	}
}

func TestInstallOpenBaoMigratesExistingHTTPListenerBeforeReturning(t *testing.T) {
	t.Parallel()
	claim := openBaoDataPVC("openbao", "node-a")
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openbao-0",
			Namespace: "openbao",
			UID:       types.UID("old-openbao-pod"),
			Labels:    map[string]string{"app.kubernetes.io/instance": "openbao"},
		},
		Spec:   corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "openbao"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao", Namespace: "openbao"},
		Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "openbao",
			Env:  []corev1.EnvVar{{Name: "BAO_ADDR", Value: "http://127.0.0.1:8200"}},
		}}}}},
	}
	oldConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-config", Namespace: "openbao"},
		Data:       map[string]string{"extraconfig-from-values.hcl": `listener "tcp" { tls_disable = 1 }`},
	}
	defaultSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "openbao"}}
	client := fake.NewClientset(claim, oldPod, statefulSet, oldConfig, defaultSA)
	var actions []string
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		if pod.Name != openBaoTLSBootstrapPodName {
			return false, nil, nil
		}
		actions = append(actions, "bootstrap@"+pod.Spec.NodeName)
		pod.Status.Phase = corev1.PodSucceeded
		return true, pod, nil
	})
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		deletion := action.(ktesting.DeleteAction)
		if deletion.GetName() != "openbao-0" {
			return false, nil, nil
		}
		actions = append(actions, "roll-openbao")
		resource := corev1.SchemeGroupVersion.WithResource("pods")
		if err := client.Tracker().Delete(resource, "openbao", "openbao-0"); err != nil {
			return true, nil, err
		}
		replacement := oldPod.DeepCopy()
		replacement.UID = types.UID("new-openbao-pod")
		if err := client.Tracker().Create(resource, replacement, "openbao"); err != nil {
			return true, nil, err
		}
		return true, nil, nil
	})
	caPEM, err := secretstore.BootstrapOpenBaoTLS(filepath.Join(t.TempDir(), "tls"), "openbao")
	if err != nil {
		t.Fatal(err)
	}
	digestImage := "registry.example/oberth@sha256:" + strings.Repeat("c", 64)
	var helmTLSArgs []string
	deps := Deps{
		Output:     io.Discard,
		KubeClient: client,
		PodLogs: func(_ context.Context, _, _ string) ([]byte, error) {
			return caPEM, nil
		},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				return []byte(`[{"name":"openbao","namespace":"openbao","status":"deployed","chart":"openbao-0.29.0"}]`), nil
			case "show":
				return []byte("image:\n  ref: " + digestImage + "\n"), nil
			case "upgrade":
				actions = append(actions, "helm-tls")
				helmTLSArgs = append([]string(nil), args...)
			}
			return nil, nil
		},
	}
	result, err := InstallOpenBao(context.Background(), Config{
		InstallSecretStore:  true,
		OpenBaoNamespace:    "openbao",
		OpenBaoChartVersion: DefaultOpenBaoChartVersion,
		Timeout:             time.Second,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upgraded || result.AlreadyInstalled {
		t.Fatalf("migration result = %+v", result)
	}
	joinedHelmArgs := strings.Join(helmTLSArgs, " ")
	if !strings.Contains(joinedHelmArgs, "--version 0.29.0") || strings.Contains(joinedHelmArgs, "--version 0.28.6") {
		t.Fatalf("TLS migration must reconcile the installed chart without downgrade: %q", joinedHelmArgs)
	}
	if helm, roll := slices.Index(actions, "helm-tls"), slices.Index(actions, "roll-openbao"); helm < 0 || roll <= helm {
		t.Fatalf("OnDelete pod must roll only after TLS Helm config is published: %v", actions)
	}
}
