package argojob

import (
	"cmp"
	"slices"

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
		return sameServerContainer(container, executorWaiterContainer(image, executorWaiterResources(false)))
	case nonrootPrepareName:
		return sameServerContainer(container, nonrootPrepareContainer(false, image)) ||
			sameServerContainer(container, nonrootPrepareContainer(true, image))
	default:
		return false
	}
}

// Admission may supply budgets and harmless API defaults. Execution inputs and
// the mount set remain exact; compare copies so validation never mutates a Pod.
func sameServerContainer(actual, expected corev1.Container) bool {
	actual = *actual.DeepCopy()
	expected = *expected.DeepCopy()
	actual.ImagePullPolicy = expected.ImagePullPolicy
	actual.TerminationMessagePath = expected.TerminationMessagePath
	actual.TerminationMessagePolicy = expected.TerminationMessagePolicy
	actual.Resources = expected.Resources
	if !serverSecurityNoWeaker(actual.SecurityContext, expected.SecurityContext) {
		return false
	}
	actual.SecurityContext = expected.SecurityContext
	less := func(a, b corev1.VolumeMount) int { return cmp.Compare(a.MountPath, b.MountPath) }
	slices.SortFunc(actual.VolumeMounts, less)
	slices.SortFunc(expected.VolumeMounts, less)
	return apiequality.Semantic.DeepEqual(actual, expected)
}

// Keep the server's restrictions. Allow explicit safe defaults and a nonroot
// identity injected by admission; unknown security changes fail closed.
func serverSecurityNoWeaker(actual, expected *corev1.SecurityContext) bool {
	if actual == nil {
		return false
	}
	safe := actual.DeepCopy()
	if safe.Privileged != nil && !*safe.Privileged {
		safe.Privileged = expected.Privileged
	}
	if safe.ProcMount != nil && *safe.ProcMount == corev1.DefaultProcMount {
		safe.ProcMount = expected.ProcMount
	}
	if safe.RunAsUser != nil && *safe.RunAsUser > 0 && expected.RunAsUser != nil && *expected.RunAsUser == 0 {
		safe.RunAsUser = expected.RunAsUser
	}
	if safe.RunAsGroup != nil && *safe.RunAsGroup > 0 && expected.RunAsGroup != nil && *expected.RunAsGroup == 0 {
		safe.RunAsGroup = expected.RunAsGroup
	}
	if safe.RunAsNonRoot != nil && *safe.RunAsNonRoot {
		safe.RunAsNonRoot = expected.RunAsNonRoot
	}
	if safe.Capabilities != nil && len(safe.Capabilities.Add) == 0 && slices.Contains(safe.Capabilities.Drop, corev1.Capability("ALL")) {
		safe.Capabilities = expected.Capabilities
	}
	if safe.AppArmorProfile != nil && safe.AppArmorProfile.Type == corev1.AppArmorProfileTypeRuntimeDefault && safe.AppArmorProfile.LocalhostProfile == nil {
		safe.AppArmorProfile = expected.AppArmorProfile
	}
	return apiequality.Semantic.DeepEqual(safe, expected)
}
