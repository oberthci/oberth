// Package retirement checks the prerequisites for removing old product surfaces.
package retirement

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// LegacyExecutionRemoved is enabled by the final removal commit. The preceding
// check can be deployed with the recovery engine still available.
const LegacyExecutionRemoved = false

// CheckSource inspects declarations without executing repository code. The
// capability decision is server-owned; a source annotation cannot assert it.
func CheckSource(path string, body []byte, operatorCapability bool) ([]string, error) {
	if path == ".oberth/tests.yaml" {
		return nil, errors.New("legacy .oberth/tests.yaml suite must migrate before execution removal")
	}
	if len(body) > 1<<20 {
		return nil, errors.New("source exceeds retirement inspection limit")
	}
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(body)))
	document, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("inspect pipeline document: %w", err)
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		return nil, errors.New("retirement inspection requires exactly one YAML document")
	}
	raw, err := yaml.YAMLToJSONStrict(document)
	if err != nil {
		return nil, fmt.Errorf("inspect pipeline YAML: %w", err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return nil, errors.New("pipeline must be a YAML object")
	}
	refs := map[string]bool{}
	var walk func(any, int) error
	walk = func(node any, depth int) error {
		if depth > 64 {
			return errors.New("retirement inspection nesting exceeds 64")
		}
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				switch key {
				case "oberth.ci/kvm-templates", "oberth.ci/guest-test-templates", "oberth.ci/trusted-tests-entrypoint", "oberth.ci/cleanup-verdict":
					return fmt.Errorf("legacy declaration %s must migrate before execution removal", key)
				case "name":
					if child == "OBERTH_KVM" || child == "OBERTH_GUEST_TEST" {
						return fmt.Errorf("legacy declaration %s must migrate before execution removal", child)
					}
				case "devices.kubevirt.io/kvm", "devices.kubevirt.io/tun", "devices.kubevirt.io/vhost-net":
					if !operatorCapability {
						return fmt.Errorf("legacy KubeVirt device %s requires a migrated operator capability", key)
					}
				case "manifest":
					if text, ok := child.(string); ok && strings.Contains(text, "kubevirt.io/") && !operatorCapability {
						return errors.New("legacy KubeVirt resource requires a migrated operator capability")
					}
				case "templateRef":
					ref, ok := child.(map[string]any)
					name, named := ref["name"].(string)
					if !ok || !named || name == "" {
						return errors.New("cannot inspect unnamed pipeline fragment")
					}
					refs[name] = true
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range value {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for name := range refs {
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}
