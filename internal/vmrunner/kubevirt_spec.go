package vmrunner

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Compare actual API fields with the closed operator construction. An echoed
// intent hash cannot authorize injected interfaces, volumes, hooks or devices.
// Unknown admission defaults fail closed until explicitly reviewed.
func compareVMISpec(path string, expected, observed any) error {
	switch fixed := expected.(type) {
	case map[string]any:
		actual, ok := observed.(map[string]any)
		if !ok {
			return fmt.Errorf("vmrunner: observed VMI differs at %s", path)
		}
		for key, value := range fixed {
			if err := compareVMISpec(path+"."+key, value, actual[key]); err != nil {
				return err
			}
		}
		for key, value := range actual {
			if _, exists := fixed[key]; !exists && !inertVMIDefault(path+"."+key, value) {
				return fmt.Errorf("vmrunner: unapproved VMI field %s.%s", path, key)
			}
		}
	case []any:
		if len(fixed) == 0 && observed == nil {
			return nil
		}
		actual, ok := observed.([]any)
		if !ok || len(actual) != len(fixed) {
			return fmt.Errorf("vmrunner: observed VMI list differs at %s", path)
		}
		for i := range fixed {
			if err := compareVMISpec(path+"[]", fixed[i], actual[i]); err != nil {
				return err
			}
		}
	default:
		if reflect.DeepEqual(expected, observed) {
			return nil
		}
		if strings.Contains(path, ".resources.") || strings.HasSuffix(path, ".emptyDisk.capacity") || path == "spec.domain.memory.guest" {
			a, aOK := expected.(string)
			b, bOK := observed.(string)
			if aOK && bOK {
				qa, ea := resource.ParseQuantity(a)
				qb, eb := resource.ParseQuantity(b)
				if ea == nil && eb == nil && qa.Cmp(qb) == 0 {
					return nil
				}
			}
		}
		return fmt.Errorf("vmrunner: observed VMI differs at %s", path)
	}
	return nil
}

func inertVMIDefault(path string, value any) bool {
	defaults := map[string]any{
		"spec.schedulerName":                                        "default-scheduler",
		"spec.dnsPolicy":                                            "ClusterFirst",
		"spec.domain.cpu.sockets":                                   int64(1),
		"spec.domain.cpu.threads":                                   int64(1),
		"spec.domain.cpu.model":                                     "host-model",
		"spec.domain.devices.disks[].disk.pciAddress":               "",
		"spec.domain.firmware.kernelBoot.container.imagePullPolicy": "IfNotPresent",
	}
	if expected, exists := defaults[path]; exists && reflect.DeepEqual(value, expected) {
		return true
	}
	if path == "spec.domain.firmware.uuid" {
		text, ok := value.(string)
		if !ok {
			return false
		}
		_, err := uuid.Parse(text)
		return err == nil
	}
	return false
}
