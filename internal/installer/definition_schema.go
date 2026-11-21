package installer

import (
	"maps"
	"slices"

	definitions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Compatibility checks the pinned API shape, not chart labels or schema bytes.
func compatibleDefinition(want, live definitions.CustomResourceDefinition) string {
	if live.Spec.Group != want.Spec.Group || live.Spec.Scope != want.Spec.Scope ||
		live.Spec.Names.Kind != want.Spec.Names.Kind || live.Spec.Names.Plural != want.Spec.Names.Plural {
		return "group, scope or resource name differs"
	}
	if !compatibleVersions(want, live) {
		return "served, storage or stored versions differ"
	}
	for _, version := range want.Spec.Versions {
		if !version.Served {
			continue
		}
		index := slices.IndexFunc(live.Spec.Versions, func(v definitions.CustomResourceDefinitionVersion) bool { return v.Name == version.Name })
		if index < 0 {
			return "served version missing: " + version.Name
		}
		if detail := definitionPayload(live.Spec.Versions[index].Schema, want.Spec.Names.Kind); detail != "" {
			return version.Name + ": " + detail
		}
	}
	return ""
}

func definitionVersions(crd definitions.CustomResourceDefinition) (map[string]bool, map[string]bool) {
	served, stored := map[string]bool{}, map[string]bool{}
	for _, v := range crd.Spec.Versions {
		if v.Served {
			served[v.Name] = true
		}
		if v.Storage {
			stored[v.Name] = true
		}
	}
	return served, stored
}

func compatibleVersions(want, live definitions.CustomResourceDefinition) bool {
	served, stored := definitionVersions(want)
	actualServed, actualStored := definitionVersions(live)
	if !maps.Equal(served, actualServed) || !maps.Equal(stored, actualStored) || len(stored) != len(live.Status.StoredVersions) {
		return false
	}
	for _, name := range live.Status.StoredVersions {
		if !stored[name] {
			return false
		}
	}
	return true
}
