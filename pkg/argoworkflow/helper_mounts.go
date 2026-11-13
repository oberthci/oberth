package argoworkflow

import (
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Authored aliases, including read-only mounts, must not replace the trusted
// helper executable or hide a sibling trust anchor. /var/run is a conventional
// image alias of /run, so reserve that spelling as well.
func admitHelperMounts(location string, mounts []corev1.VolumeMount, devices []corev1.VolumeDevice) []error {
	var problems []error
	for i, mount := range mounts {
		problems = append(problems, admitExecutorVolumeName(fmt.Sprintf("%s.volumeMounts[%d]", location, i), mount.Name)...)
		if helperPathMayBeShadowed(mount.MountPath) {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.volumeMounts[%d].mountPath %q uses a reserved server helper path; Oberth helper and executor identity paths are reserved", location, i, mount.MountPath))
		}
	}
	for i, device := range devices {
		problems = append(problems, admitExecutorVolumeName(fmt.Sprintf("%s.volumeDevices[%d]", location, i), device.Name)...)
		if helperPathMayBeShadowed(device.DevicePath) {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.volumeDevices[%d].devicePath %q uses a reserved server helper path; Oberth helper and executor identity paths are reserved", location, i, device.DevicePath))
		}
	}
	return problems
}

func helperPathMayBeShadowed(value string) bool {
	// Runtime substitution can introduce both separators and parent segments;
	// a literal prefix outside the reserved root does not make it safe.
	if strings.Contains(value, "{{") || strings.Contains(value, "}}") {
		return true
	}
	cleaned := path.Clean(value)
	for _, root := range []string{"/run/oberth", "/var/run/oberth", ExecutorIdentityMountPath, "/run/secrets/kubernetes.io/serviceaccount"} {
		if cleaned == root || strings.HasPrefix(cleaned, root+"/") {
			return true
		}
	}
	return false
}
