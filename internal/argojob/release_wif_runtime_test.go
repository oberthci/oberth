package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func runtimeGrant(repo, body string) model.ReleaseWIFOverride {
	return model.ReleaseWIFOverride{Repo: repo, Grant: json.RawMessage(body)}
}

// A runtime grant pinned in the request authorizes a release leaf the boot map
// does not list, and the policy digest covers the effective map (#1497).
func TestReleaseWIFRuntimeGrantPinnedInRequestAuthorizesBuild(t *testing.T) {
	config := wifTestConfig()
	delete(config.ReleaseWIF.Repositories, "github/acme/oberth")
	request := wifTestRequest(t, nil)
	if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("boot map without the repository must refuse: %v", err)
	}
	runtime := NewReleaseWIFRuntime(config)
	if inactive := runtime.Apply([]model.ReleaseWIFOverride{runtimeGrant("github/acme/oberth", `{"service_account_name":"oberth-per-repo","templates":{"main":"image-writer"}}`)}); len(inactive) != 0 {
		t.Fatalf("valid grant inactive: %+v", inactive)
	}
	request.ReleaseWIF = runtime.Snapshot()
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.Templates[0].ServiceAccountName != "oberth-per-repo" || wf.Annotations["oberth.ci/release-wif-policy-sha256"] == "" {
		t.Fatal("runtime grant did not bind the release leaf")
	}
	if config.ReleaseWIF.Repositories["github/acme/oberth"].Templates != nil || runtime.Base().Repositories["github/acme/oberth"].Templates != nil {
		t.Fatal("runtime grant mutated the boot map")
	}
	preflight, err := PreflightRelease(config, request)
	if err != nil || len(preflight.Problems) != 0 {
		t.Fatalf("preflight with runtime grant: %+v %v", preflight, err)
	}
}

func TestReleaseWIFRuntimeRevokeRemovesBootEntry(t *testing.T) {
	config := wifTestConfig()
	runtime := NewReleaseWIFRuntime(config)
	runtime.Apply([]model.ReleaseWIFOverride{{Repo: "github/acme/oberth", Revoked: true}})
	request := wifTestRequest(t, nil)
	request.ReleaseWIF = runtime.Snapshot()
	if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("revoked boot entry still authorizes: %v", err)
	}
	if state := runtime.State(); state.Boot["github/acme/oberth"] == nil || state.Effective["github/acme/oberth"] != nil {
		t.Fatalf("state = %+v", state)
	}
	request.ReleaseWIF = nil
	if _, err := Build(config, request); err != nil {
		t.Fatalf("boot map must remain the default: %v", err)
	}
}

func TestReleaseWIFRuntimeRefusesWideningGrants(t *testing.T) {
	config := wifTestConfig()
	runtime := NewReleaseWIFRuntime(config)
	repo := "github/acme/other"
	for name, body := range map[string]string{
		"dedicated step account": `{"service_account_name":"other-release","templates":{"main":"image-writer"},"step_service_accounts":{"main":"wif-only"}}`,
		"unknown role":           `{"service_account_name":"other-release","templates":{"main":"cluster-admin"}}`,
		"shared account":         `{"service_account_name":"` + config.CredentialedServiceAccount + `","templates":{"main":"image-writer"}}`,
		"another repository SA":  `{"service_account_name":"oberth-per-repo","templates":{"main":"image-writer"}}`,
		"no account":             `{"templates":{"main":"image-writer"}}`,
		"no templates":           `{"service_account_name":"other-release","templates":{}}`,
		"trailing document":      `{"service_account_name":"other-release","templates":{"main":"image-writer"}} {}`,
	} {
		overrides := []model.ReleaseWIFOverride{runtimeGrant(repo, body)}
		if err := runtime.Check(overrides); err == nil {
			t.Fatalf("%s accepted", name)
		}
		if inactive := runtime.Apply(overrides); len(inactive) != 1 || inactive[0].Repo != repo {
			t.Fatalf("%s: inactive = %+v", name, inactive)
		}
		if _, ok := runtime.Snapshot().Repositories[repo]; ok {
			t.Fatalf("%s widened the effective map", name)
		}
	}
	// A revoke applies even when a stored grant is invalid.
	runtime.Apply([]model.ReleaseWIFOverride{{Repo: "github/acme/oberth", Revoked: true}, runtimeGrant(repo, `{"templates":{"main":"cluster-admin"}}`)})
	if len(runtime.Snapshot().Repositories) != 0 {
		t.Fatal("revocation skipped alongside an invalid grant")
	}
}

func TestReleaseWIFRuntimeWithoutBootMapStaysDisabled(t *testing.T) {
	config := testConfig()
	runtime := NewReleaseWIFRuntime(config)
	if err := runtime.Check([]model.ReleaseWIFOverride{runtimeGrant("github/acme/oberth", `{"service_account_name":"oberth-per-repo","templates":{"main":"image-writer"}}`)}); err == nil {
		t.Fatal("runtime grant enabled federation without boot roles")
	}
	if runtime.Snapshot() != nil || runtime.State().Enabled {
		t.Fatal("disabled federation gained a runtime map")
	}
}

// Admission refuses a release identity that is another repository's release
// or any CI identity, even when that account is outside the WIF map, so the
// runtime check refuses it before the grant is stored (#1497).
func TestReleaseWIFRuntimeRefusesOtherRepositoryIdentitiesOutsideMap(t *testing.T) {
	config := wifTestConfig()
	delete(config.ReleaseWIF.Repositories, "github/acme/oberth")
	runtime := NewReleaseWIFRuntime(config)
	repo := "github/acme/other"
	grant := func(account string) []model.ReleaseWIFOverride {
		return []model.ReleaseWIFOverride{runtimeGrant(repo, `{"service_account_name":"`+account+`","templates":{"main":"image-writer"}}`)}
	}
	if err := runtime.Check(grant("oberth-per-repo")); err == nil || !strings.Contains(err.Error(), "github/acme/oberth") {
		t.Fatalf("boot release identity of another repository accepted: %v", err)
	}
	if err := runtime.Check(grant("other-release")); err != nil {
		t.Fatalf("unrelated account refused: %v", err)
	}
	// The live identity store replaces the boot identity maps.
	runtime.SetIdentityStore(NewIdentityStoreWithSteps(
		map[string]PerRepoIdentityConfig{"github/acme/third": {ServiceAccountName: "third-release"}, repo: {ServiceAccountName: "other-release"}},
		map[string]PerRepoIdentityConfig{repo: {ServiceAccountName: "other-ci"}}, nil))
	for _, account := range []string{"third-release", "other-ci"} {
		if err := runtime.Check(grant(account)); err == nil {
			t.Fatalf("%s accepted", account)
		}
		if inactive := runtime.Apply(grant(account)); len(inactive) != 1 {
			t.Fatalf("%s applied: %+v", account, inactive)
		}
	}
	if err := runtime.Check(grant("oberth-per-repo")); err != nil {
		t.Fatalf("account released by the identity store still refused: %v", err)
	}
	if err := runtime.Check(grant("other-release")); err != nil {
		t.Fatalf("repository's own release identity refused: %v", err)
	}
}
