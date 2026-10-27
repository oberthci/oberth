package installer

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/containerimage"
)

func TestRestoreHelperSeparatesServerSchemaFromFileTools(t *testing.T) {
	server := serverDeployment{Image: "example.invalid/scratch@sha256:fixture", ClaimName: "data"}
	pod := restoreHelperPod("restore", "oberth", "stamp", server)
	if len(pod.Spec.InitContainers) != 1 || len(pod.Spec.Containers) != 1 {
		t.Fatal("missing schema or utility container")
	}
	for _, container := range pod.Spec.InitContainers {
		if container.Image == server.Image && (len(container.Command) == 0 || container.Command[0] != "/usr/local/bin/oberth") {
			t.Fatalf("non-native server helper: %+v", container)
		}
	}
	if pod.Spec.InitContainers[0].Image != server.Image || pod.Spec.InitContainers[0].Args[0] != "version" {
		t.Fatal("schema probe doesn't use target server image")
	}
	if pod.Spec.Containers[0].Image != containerimage.Utility {
		t.Fatal("restore shell uses server image")
	}
}

func TestUtilityImageOverrideReachesMaintenanceHelpers(t *testing.T) {
	mirror := "registry.internal/busybox@sha256:fixture"
	server := serverDeployment{Image: "registry.internal/oberth@sha256:fixture", UtilityImage: mirror, ClaimName: "data"}
	pod := restoreHelperPod("restore", "oberth", "stamp", server)
	if pod.Spec.Containers[0].Image != mirror {
		t.Fatal("restore lost utility mirror")
	}
	args := OberthHelmArgs(Config{UtilityImage: mirror}, OpenBaoResult{}, RekorResult{})
	found := false
	for _, arg := range args {
		if arg == "utilityImage="+mirror {
			found = true
		}
	}
	if !found {
		t.Fatalf("installer lost utility mirror: %v", args)
	}
}

func TestMaintenanceUtilityImageComesFromInstalledDeployment(t *testing.T) {
	for _, mirror := range []string{"registry.internal/busybox@sha256:fixture", "registry.internal/oberth@sha256:fixture"} {
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth"}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "oberth"}}, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "prepare-caches", Image: mirror}},
			Containers:     []corev1.Container{{Name: "oberth", Image: "registry.internal/oberth@sha256:fixture"}},
			Volumes:        []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}},
		}}}}
		deps := Deps{KubeClient: fake.NewClientset(deployment)}
		server, err := inspectServerDeployment(t.Context(), deps, "oberth")
		if err != nil {
			t.Fatal(err)
		}
		pod := restoreHelperPod("helper", "oberth", "stamp", server)
		want := mirror
		if mirror == server.Image {
			want = containerimage.Utility
		}
		if got := pod.Spec.Containers[0].Image; got != want {
			t.Fatalf("maintenance utility image=%s want=%s", got, want)
		}
	}
}
