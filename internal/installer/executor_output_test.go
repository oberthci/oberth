package installer_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
)

func TestBundledControllerSuppressesExecutorInfoOutput(t *testing.T) {
	t.Parallel()
	objects := rendered(t, nil)
	var controller appsv1.Deployment
	decodeObject(t, object(t, objects, "Deployment", "oberth-controller-workflow-controller"), &controller)
	args := controller.Spec.Template.Spec.Containers[0].Args
	for i, arg := range args {
		if arg == "--loglevel" && i+1 < len(args) && args[i+1] == "warn" {
			return
		}
	}
	t.Fatal("executor informational output is enabled", args)
}
