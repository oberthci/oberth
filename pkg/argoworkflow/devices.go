package argoworkflow

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// deviceGrant is the grant kind that lets one step request one extended
// resource, such as a device plugin's device.
const deviceGrant = "device"

// standardResource reports the container resources any step may request; every
// other name is an extended resource, which a device plugin can back with a
// host device.
func standardResource(name corev1.ResourceName) bool {
	switch name {
	case corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
		return true
	}
	return strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix)
}

// devices requires an exact repository, file and step grant for each extended
// resource a container requests or limits. Template defaults are checked in
// the steps they are merged into.
func (g *graph) devices(c *corev1.Container, n *node, where string) {
	if n.isDefault && n.scope == "" {
		return
	}
	names := map[corev1.ResourceName]bool{}
	for name := range c.Resources.Limits {
		names[name] = true
	}
	for name := range c.Resources.Requests {
		names[name] = true
	}
	for _, name := range sortedKeys(names) {
		if standardResource(name) || g.granted(n, deviceGrant, string(name), "") {
			continue
		}
		g.missingGrant(n, deviceGrant, string(name), where+".resources["+string(name)+"]")
	}
}
