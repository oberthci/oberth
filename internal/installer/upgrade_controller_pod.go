package installer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// verifyControllerServer waits out old/terminating rollout Pods, then binds the
// version execution to the unique ready Pod and re-observes its same UID and
// controller/template chain. Kubernetes observation is not an atomic lock.
func verifyControllerServer(ctx context.Context, cfg UpgradeConfig, deps Deps, deployment *appsv1.Deployment) error {
	deadline, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	var pod *corev1.Pod
	for {
		var err error
		pod, err = controllerServerPod(deadline, cfg, deps, deployment)
		if err == nil {
			break
		}
		select {
		case <-deadline.Done():
			return errors.New("oberth controller-profile ready Pod readback refused")
		case <-time.After(pollInterval(deps)):
		}
	}
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	args := []string{"exec", "-c", "oberth", "-n", cfg.Namespace, pod.Name, "--", "oberth", "version"}
	if deps.ContextName != "" {
		args = append([]string{"--context", deps.ContextName}, args...)
	}
	out, err := run(deadline, nil, "kubectl", args...)
	if err != nil {
		return errors.New("oberth fixed Pod version readback failed")
	}
	version := ""
	for _, field := range strings.Fields(string(out)) {
		if strings.HasPrefix(field, "v") && semver.IsValid(field) {
			if version != "" {
				return errors.New("oberth fixed Pod version ambiguous")
			}
			version = field
		}
	}
	if version == "" || semver.Compare(version, canonicalChartVersion(cfg.BinaryVersion)) != 0 {
		return errors.New("oberth fixed Pod version mismatch")
	}
	fresh, err := controllerServerPod(deadline, cfg, deps, deployment)
	if err != nil || fresh.Name != pod.Name || fresh.UID != pod.UID {
		return errors.New("oberth fixed Pod changed during version readback")
	}
	return nil
}

func controllerServerPod(ctx context.Context, cfg UpgradeConfig, deps Deps, expected *appsv1.Deployment) (*corev1.Pod, error) {
	refused := errors.New("oberth Pod ownership, template or readiness refused")
	deployment, err := deps.KubeClient.AppsV1().Deployments(cfg.Namespace).Get(ctx, expected.Name, metav1.GetOptions{})
	if err != nil || deployment.UID == "" || deployment.UID != expected.UID || deployment.Generation != expected.Generation || !apiequality.Semantic.DeepEqual(deployment.Spec, expected.Spec) || deployment.Spec.Selector == nil {
		return nil, refused
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil || selector.Empty() {
		return nil, refused
	}
	pods, err := deps.KubeClient.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	// An old terminating Pod means rollout ownership is still ambiguous. Wait;
	// never silently choose one of multiple candidates.
	if err != nil || len(pods.Items) != 1 {
		return nil, refused
	}
	pod := &pods.Items[0]
	if pod.UID == "" || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return nil, refused
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "ReplicaSet" || owner.APIVersion != "apps/v1" || owner.UID == "" {
		return nil, refused
	}
	rs, err := deps.KubeClient.AppsV1().ReplicaSets(cfg.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil || rs.UID != owner.UID || rs.DeletionTimestamp != nil {
		return nil, refused
	}
	rsOwner := metav1.GetControllerOf(rs)
	if rsOwner == nil || rsOwner.Kind != "Deployment" || rsOwner.APIVersion != "apps/v1" || rsOwner.Name != deployment.Name || rsOwner.UID != deployment.UID {
		return nil, refused
	}
	template := rs.Spec.Template.DeepCopy()
	delete(template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	if !apiequality.Semantic.DeepEqual(*template, deployment.Spec.Template) {
		return nil, refused
	}
	if !deploymentRolledOut(deployment, serverContainerImage(deployment.Spec.Template.Spec.Containers)) || len(pod.Spec.Containers) != len(deployment.Spec.Template.Spec.Containers) {
		return nil, refused
	}
	for _, expectedContainer := range deployment.Spec.Template.Spec.Containers {
		matched := false
		for _, actual := range pod.Spec.Containers {
			if actual.Name == expectedContainer.Name {
				matched = actual.Image == expectedContainer.Image && slices.Equal(actual.Command, expectedContainer.Command) && slices.Equal(actual.Args, expectedContainer.Args)
			}
		}
		if !matched {
			return nil, refused
		}
	}
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	serverReady := false
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "oberth" && status.Ready && status.State.Running != nil && status.ImageID != "" {
			serverReady = true
		}
	}
	if !ready || !serverReady {
		return nil, refused
	}
	return pod, nil
}

func serverContainerImage(containers []corev1.Container) string {
	for _, container := range containers {
		if container.Name == "oberth" {
			return container.Image
		}
	}
	return ""
}
