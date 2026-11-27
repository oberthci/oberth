package installer

import (
	_ "embed"
	"fmt"
	"reflect"
	"sort"

	"sigs.k8s.io/yaml"
)

//go:embed kubelet-profile.yaml
var hostProfile string

// HostProfile returns the operator-applied kubelet drop-in, outside Helm
// ownership, preserving this measured profile's daemon reserves and Pod ceiling.
func HostProfile() string { return hostProfile }

// ReviewHost compares a captured effective configz kubeletconfig with the
// proposed profile. It neither reads a node nor changes its configuration.
func ReviewHost(effective map[string]any) []string {
	var expected map[string]any
	_ = yaml.Unmarshal([]byte(hostProfile), &expected)
	var changed []string
	for name, want := range expected {
		if name == "apiVersion" || name == "kind" {
			continue
		}
		if !reflect.DeepEqual(effective[name], want) {
			changed = append(changed, name)
		}
	}
	// This profile preserves the existing unsplit reserve, including no second
	// kube reserve. A future measured split needs a separately reviewed profile.
	if reserve, ok := effective["kubeReserved"]; ok && reserve != nil && !reflect.DeepEqual(reserve, map[string]any{}) {
		changed = append(changed, "kubeReserved")
	}
	sort.Strings(changed)
	if len(changed) == 0 {
		return []string{"Host profile matches captured effective configuration; G1/G7 still required."}
	}
	lines := []string{"Host profile differs; drained host maintenance required."}
	for _, name := range changed {
		lines = append(lines, fmt.Sprintf("Host field: %s", name))
	}
	return lines
}
