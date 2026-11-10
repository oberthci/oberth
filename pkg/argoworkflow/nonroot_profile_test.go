package argoworkflow

import (
	"strings"
	"testing"
)

// The zero-limit profile is exactly the pinned built-in one, so existing
// installs keep their ConfigMap and verification binding.
func TestNewNonrootProfileWithoutLimitsIsTheBuiltIn(t *testing.T) {
	profile, err := NewNonrootProfile(0, 0)
	if err != nil || profile.Config != NonrootProfileConfig || profile.SHA256 != NonrootProfileConfigSHA256 || profile.ConfigMap != NonrootProfileConfigMap {
		t.Fatalf("base profile = %+v, %v", profile, err)
	}
}

// Parallelism 6 lands in the controller config, deterministically, under a
// digest-derived name, and only the exact canonical bytes parse back (#1517).
func TestNonrootProfileCarriesParallelism(t *testing.T) {
	profile, err := NewNonrootProfile(6, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"failedPodRestart":{"enabled":false,"maxRestarts":3},"nodeEvents":{"enabled":true},"parallelism":6,"workflowEvents":{"enabled":true}}`
	if profile.Config != want || profile.ConfigMap == NonrootProfileConfigMap || !ValidNonrootProfileConfigMap(profile.ConfigMap) {
		t.Fatalf("profile = %+v", profile)
	}
	again, _ := NewNonrootProfile(6, 0)
	if again != profile {
		t.Fatal("profile is not deterministic")
	}
	both, _ := NewNonrootProfile(6, 4)
	if !strings.Contains(both.Config, `"namespaceParallelism":4`) || !strings.Contains(both.Config, `"parallelism":6`) {
		t.Fatalf("both limits = %s", both.Config)
	}
	if parsed, err := ParseNonrootProfile(profile.ConfigMap, profile.Config); err != nil || parsed != profile {
		t.Fatalf("parse = %+v, %v", parsed, err)
	}
	for name, config := range map[string]string{
		"other name":       NonrootProfileConfig,
		"spacing":          strings.ReplaceAll(profile.Config, ":", ": "),
		"extra key":        strings.Replace(profile.Config, `"parallelism":6`, `"parallelism":6,"artifactRepository":{}`, 1),
		"changed built-in": strings.Replace(profile.Config, `"maxRestarts":3`, `"maxRestarts":4`, 1),
		"zero written":     strings.Replace(profile.Config, `"parallelism":6`, `"parallelism":0`, 1),
	} {
		if _, err := ParseNonrootProfile(profile.ConfigMap, config); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := NewNonrootProfile(-1, 0); err == nil {
		t.Fatal("negative parallelism accepted")
	}
	if ValidNonrootProfileConfigMap("oberth-argo-nr-XYZ") || ValidNonrootProfileConfigMap("other") {
		t.Fatal("malformed name accepted")
	}
}
