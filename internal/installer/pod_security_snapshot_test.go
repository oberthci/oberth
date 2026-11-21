package installer_test

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestPipelinePodSecurityUsesNamespaceSnapshot(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"prepare", "revalidate"} {
		t.Run(phase, func(t *testing.T) { checkPipelineNamespaceSnapshot(t, phase) })
	}
}

func checkPipelineNamespaceSnapshot(t *testing.T, phase string) {
	t.Helper()
	f := setup(t)
	pipelineNamespaceLevel(t, f, "baseline")
	var plan *installer.Plan
	if phase == "revalidate" {
		plan = prepare(t, f)
	}
	reads := 0
	f.client.PrependReactor("get", "namespaces", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.(ktesting.GetAction).GetName() != "oberth-runs" {
			return false, nil, nil
		}
		reads++
		level := "baseline"
		if reads > 1 {
			level = "privileged"
		}
		return true, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "oberth-runs", Labels: map[string]string{"pod-security.kubernetes.io/enforce": level}}}, nil
	})
	if phase == "prepare" {
		plan = prepare(t, f)
	} else {
		stop := errors.New("stop after revalidation")
		_, err := f.engine.Apply(t.Context(), plan, plan.Digest(), func(p installer.Phase) error {
			if p == installer.Creating {
				return stop
			}
			return nil
		})
		if !errors.Is(err, stop) {
			t.Fatal(err)
		}
	}
	defer plan.Close()
	if reads != 1 {
		t.Fatalf("pipeline namespace read %d times; validation and creation must share one snapshot", reads)
	}
}
