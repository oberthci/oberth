package installer

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLegacyIdentityUpgradeStopsBeforeRotation(t *testing.T) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth"}}
	deployment.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "host-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "existing-host-key"}}}}
	kube := fake.NewClientset(deployment)
	err := rejectLegacySecretDeployment(context.Background(), Config{}, Deps{KubeClient: kube})
	if err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("legacy identity was not protected: %v", err)
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() != "get" {
			t.Fatal("migration check mutated cluster")
		}
	}
	deployment.Spec.Template.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}
	if err := rejectLegacySecretDeployment(context.Background(), Config{}, Deps{KubeClient: fake.NewClientset(deployment)}); err != nil {
		t.Fatal(err)
	}
	if err := rejectLegacySecretDeployment(context.Background(), Config{}, Deps{KubeClient: fake.NewClientset()}); err != nil {
		t.Fatal(err)
	}
}

func TestReadyCloneInstructionsIncludeQualifiedPath(t *testing.T) {
	for _, alias := range []bool{false, true} {
		var out strings.Builder
		printReadyWithNextSteps(&out, "github", "ssh://git@github.com/oberthci", alias, false)
		if !strings.Contains(out.String(), "github/oberthci/<repo>.git") {
			t.Fatal(out.String())
		}
	}
}
