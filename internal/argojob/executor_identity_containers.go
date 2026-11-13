package argojob

import (
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
)

func executorWaiterContainer(image string, resources corev1.ResourceRequirements) corev1.Container {
	return corev1.Container{
		Name: executorTokenWaiter, Image: image,
		Command:         []string{"sh", "-c", executorTokenWaitCommand},
		SecurityContext: nonrootContainerSecurity(0), Resources: resources,
		VolumeMounts: []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}},
	}
}

// Match the very same constructors used to produce the Pod patch. Strategic
// merge preserves unnamed fields and additional mounts from an authored helper
// with the same name, so checking only the patched image/command is unsafe.
func validServerContainer(container corev1.Container, image string, initializer bool) bool {
	if !initializer {
		return false
	}
	switch container.Name {
	case executorTokenWaiter:
		return sameServerContainer(container, executorWaiterContainer(image, executorWaiterResources(false))) ||
			sameServerContainer(container, executorWaiterContainer(image, executorWaiterResources(true)))
	case nonrootPrepareName:
		return sameServerContainer(container, nonrootPrepareContainer(false, image)) ||
			sameServerContainer(container, nonrootPrepareContainer(true, image))
	default:
		return false
	}
}

func sameServerContainer(actual, expected corev1.Container) bool {
	// These are the three defaults Kubernetes adds to a Container. Normalize
	// omitted defaults on either copy; every other field must match exactly.
	defaults := func(container *corev1.Container) {
		if container.ImagePullPolicy == "" {
			container.ImagePullPolicy = corev1.PullIfNotPresent
		}
		if container.TerminationMessagePath == "" {
			container.TerminationMessagePath = corev1.TerminationMessagePathDefault
		}
		if container.TerminationMessagePolicy == "" {
			container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
		}
	}
	defaults(&actual)
	defaults(&expected)
	return apiequality.Semantic.DeepEqual(actual, expected)
}
