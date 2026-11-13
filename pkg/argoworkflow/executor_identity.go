package argoworkflow

import (
	"fmt"
	"strings"
)

// ExecutorTokenVolumeName and ExecutorTokenMountPath belong exclusively to
// the server-delivered Argo executor identity, never authored containers.
const (
	// #nosec G101 -- reserved runtime volume name, not credential material.
	ExecutorTokenVolumeName = "exec-sa-token"
	// #nosec G101 -- reserved runtime directory, not credential material.
	ExecutorTokenMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"
)

func admitExecutorContainerName(location, name string, main bool) []error {
	if strings.Contains(name, "{{") || name == "init" || name == "wait" || (name == "main" && !main) {
		return []error{fmt.Errorf("argoworkflow: %s.name %q is reserved for the Argo executor; container names must be literal and helpers cannot use init, wait or main", location, name)}
	}
	return nil
}

func admitExecutorVolumeName(location, name string) []error {
	if name == ExecutorTokenVolumeName || strings.Contains(name, "{{") {
		return []error{fmt.Errorf("argoworkflow: %s.name %q can select the reserved executor identity volume and is not admitted", location, name)}
	}
	return nil
}
