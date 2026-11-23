package installer_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
)

func TestControllerRequeueInterval(t *testing.T) {
	t.Parallel()
	var controller appsv1.Deployment
	decodeObject(t, object(t, rendered(t, nil), "Deployment", "oberth-controller-workflow-controller"), &controller)
	for _, env := range controller.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "DEFAULT_REQUEUE_TIME" {
			if env.Value != "2s" || env.ValueFrom != nil {
				t.Fatalf("controller requeue = %+v; want 2s", env)
			}
			return
		}
	}
	t.Fatal("controller requeue is unset; native default delays each step boundary by 10s")
}
