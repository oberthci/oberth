package installer

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/podpriority"
)

func controlPlanePriority(cfg Config) string {
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return podpriority.ControlPlaneClass(namespace, "oberth")
}

// Establish the server and helper classes with final Helm ownership so the
// Oberth chart adopts them during installation. Dependencies use upstream defaults.
func ensurePlatformPriorities(ctx context.Context, cfg Config, deps Deps) error {
	if deps.KubeClient == nil {
		return errors.New("priority bootstrap requires Kubernetes access")
	}
	namespace, pipelineNamespace := cfg.Namespace, cfg.ArgoNamespace
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if pipelineNamespace == "" {
		pipelineNamespace = DefaultArgoNamespace
	}
	for _, desired := range []*schedulingv1.PriorityClass{
		{ObjectMeta: metav1.ObjectMeta{Name: controlPlanePriority(cfg)}, Value: podpriority.ControlPlaneValue, PreemptionPolicy: ptr.To(corev1.PreemptLowerPriority)},
		{ObjectMeta: metav1.ObjectMeta{Name: podpriority.PipelineClass(pipelineNamespace)}, Value: podpriority.PipelineValue, PreemptionPolicy: ptr.To(corev1.PreemptNever)},
	} {
		desired.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
		desired.Annotations = map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": namespace, "helm.sh/resource-policy": "keep"}
		classes := deps.KubeClient.SchedulingV1().PriorityClasses()
		current, err := classes.Get(ctx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			current, err = classes.Create(ctx, desired, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				current, err = classes.Get(ctx, desired.Name, metav1.GetOptions{})
			}
		}
		if err != nil {
			return fmt.Errorf("establish scheduling priority %s: %w", desired.Name, err)
		}
		if current.DeletionTimestamp != nil || current.Value != desired.Value || current.GlobalDefault || current.PreemptionPolicy == nil || *current.PreemptionPolicy != *desired.PreemptionPolicy || current.Labels["app.kubernetes.io/managed-by"] != "Helm" || current.Annotations["meta.helm.sh/release-name"] != "oberth" || current.Annotations["meta.helm.sh/release-namespace"] != namespace {
			return fmt.Errorf("PriorityClass %s has different policy or ownership; refusing to overwrite it", desired.Name)
		}
	}
	return nil
}
