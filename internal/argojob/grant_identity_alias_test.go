package argojob

import (
	"strings"
	"testing"
)

func TestGrantIdentityAliasesAreRefused(t *testing.T) {
	for _, tier := range []string{"release", "ci"} {
		t.Run("capability/"+tier, func(t *testing.T) {
			capability := operatorCapability()
			capability.ServiceAccount = "granted-step-account"
			config := testConfig()
			config.PerStepIdentities = map[string]PerRepoIdentityConfig{
				"upstream/org/other/.oberth/build.yaml/build": aliasIdentity(tier, capability.ServiceAccount),
			}
			if _, err := Build(config, requestWithCapability(t, operatorResourceDocument, capability)); err == nil || !strings.Contains(err.Error(), "secret-bound identity") {
				t.Fatalf("operator capability accepted %s secret identity: %v", tier, err)
			}
		})
		t.Run("wif/"+tier, func(t *testing.T) {
			config := genericWIFConfig()
			request := genericWIFRequest(t, nil)
			request.Identities = NewIdentityStoreWithSteps(config.PerRepoIdentities, config.PerRepoCIIdentities, map[string]PerRepoIdentityConfig{
				"upstream/org/other/.oberth/release.yaml/publish": aliasIdentity(tier, "release-step-main"),
			}).Snapshot()
			if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "another step's ServiceAccount") {
				t.Fatalf("WIF accepted %s secret identity: %v", tier, err)
			}
		})
	}
	// The same leaf's CI identity is also secret-bound, even when its release
	// identity is allowed by the existing same-leaf exception.
	t.Run("wif/same-leaf-ci", func(t *testing.T) {
		config := genericWIFConfig()
		request := genericWIFRequest(t, nil)
		repo := canonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repo)
		request.Identities = NewIdentityStoreWithSteps(nil, nil, map[string]PerRepoIdentityConfig{
			repo + "/main": {CIServiceAccountName: "release-step-main"},
		}).Snapshot()
		if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "another step's ServiceAccount") {
			t.Fatalf("WIF accepted same-leaf CI secret identity: %v", err)
		}
	})
}

func aliasIdentity(tier, account string) PerRepoIdentityConfig {
	if tier == "ci" {
		return PerRepoIdentityConfig{CIServiceAccountName: account}
	}
	return PerRepoIdentityConfig{ServiceAccountName: account}
}
