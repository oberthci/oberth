package installer

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ControllerDeployment is argoNative.fullnameOverride plus the subchart's
// controller name: the workflow controller Deployment the chart renders.
const ControllerDeployment = "oberth-controller-workflow-controller"

// ControllerRestart is printed after an upgrade or rollback whose apply changed
// the workflow controller's Pod template; the readiness wait has by then seen
// the new controller rolled out.
const ControllerRestart = "The workflow controller was restarted."

// WatchController reads the live controller Pod template of the profile's
// installation now; the returned function reads it again and reports whether
// it changed. A template that cannot be read on either side reports no change.
func (e *Engine) WatchController(ctx context.Context) func() bool {
	namespace, err := e.installationNamespace("")
	if err != nil {
		return func() bool { return false }
	}
	return e.watchController(ctx, namespace)
}

func (e *Engine) watchController(ctx context.Context, namespace string) func() bool {
	before, known := e.controllerTemplate(ctx, namespace)
	return func() bool {
		after, ok := e.controllerTemplate(ctx, namespace)
		return known && ok && !equality.Semantic.DeepEqual(before, after)
	}
}

func (e *Engine) controllerTemplate(ctx context.Context, namespace string) (corev1.PodTemplateSpec, bool) {
	d, err := e.Cluster.Client.AppsV1().Deployments(namespace).Get(ctx, ControllerDeployment, metav1.GetOptions{})
	if err != nil {
		return corev1.PodTemplateSpec{}, false
	}
	return d.Spec.Template, true
}

// waitController waits, within the caller's readiness deadline, until the
// status Kubernetes keeps for the controller Deployment shows the changed
// template rolled out, so no Pod of the earlier controller still serves.
func (e *Engine) waitController(ctx context.Context, namespace string) error {
	for {
		d, err := e.Cluster.Client.AppsV1().Deployments(namespace).Get(ctx, ControllerDeployment, metav1.GetOptions{})
		if err == nil && rolledOut(d) {
			return nil
		}
		if err = e.pause(ctx); err != nil {
			return problem("controller-timeout", err)
		}
	}
}

// rolledOut holds when the current generation is observed and every replica,
// and no other, runs the current template and is available.
func rolledOut(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation && s.Replicas == want && s.UpdatedReplicas == want && s.AvailableReplicas == want
}
