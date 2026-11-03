package argojob

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestExecutorScratchDefaultAndOverride(t *testing.T) {
	for _, size := range []string{"", "4Gi", "1024Mi", "1.5Gi"} {
		want := size
		if want == "" {
			want = "1Gi"
		}
		for _, script := range []bool{false, true} {
			shape := "container"
			if script {
				shape = "script"
			}
			wf := nonrootBuiltWithScratch(t, script, size)
			pod := nonrootApplyControllerFixture(t, nonrootControllerFixture(t, shape+"-before-patch.json"), wf)
			assertExecutorScratch(t, pod, want)
		}
	}
}

func assertExecutorScratch(t *testing.T, pod *corev1.Pod, want string) {
	t.Helper()
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "var-run-argo" {
			if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.Cmp(resource.MustParse(want)) != 0 || volume.EmptyDir.Medium != "" {
				t.Fatalf("executor scratch = %#v; want disk-backed %s", volume, want)
			}
			return
		}
	}
	t.Fatal("missing executor scratch volume")
}

func TestExecutorScratchRejectsInvalidLimits(t *testing.T) {
	for _, size := range []string{"invalid", "0", "-1Gi", "2Pi", " "} {
		config := testConfig()
		config.ExecutorScratchSizeLimit = size
		if err := config.Validate(); err == nil {
			t.Fatalf("accepted invalid executor scratch limit %q", size)
		}
	}
}
