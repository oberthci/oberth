package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSyncGrantPoliciesCreatesNewPolicies(t *testing.T) {
	t.Parallel()

	name := PerRepoName("codeberg", "oberthci", "oberth")
	ciName := PerRepoCIName("codeberg", "oberthci", "oberth")

	responses := map[string]fakeBaoResponse{
		// Shared policies do not exist yet.
		"policy read " + defaultCredentialedPolicy:         {out: "No policy named: " + defaultCredentialedPolicy, err: errors.New("exit status 2")},
		"policy write " + defaultCredentialedPolicy + " -": {out: "Success!"},
		"policy read " + defaultCISecretsPolicy:            {out: "No policy named: " + defaultCISecretsPolicy, err: errors.New("exit status 2")},
		"policy write " + defaultCISecretsPolicy + " -":    {out: "Success!"},
		// Per-repo identities do not exist yet.
		"policy read " + name:                            {out: "No policy named: " + name, err: errors.New("exit status 2")},
		"policy write " + name + " -":                    {out: "Success!"},
		"read -format=json auth/kubernetes/role/" + name: {out: "No value found at auth/kubernetes/role/" + name, err: errors.New("exit status 2")},
		"write auth/kubernetes/role/" + name + " -":      {out: "Success!"},
		// CI per-repo identity
		"policy read " + ciName:                            {out: "No policy named: " + ciName, err: errors.New("exit status 2")},
		"policy write " + ciName + " -":                    {out: "Success!"},
		"read -format=json auth/kubernetes/role/" + ciName: {out: "No value found at auth/kubernetes/role/" + ciName, err: errors.New("exit status 2")},
		"write auth/kubernetes/role/" + ciName + " -":      {out: "Success!"},
	}
	runner := &fakeBaoRunner{t: t, responses: responses}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	identities := []PerRepoIdentity{{
		Upstream: "codeberg",
		Org:      "oberthci",
		Repo:     "oberth",
		Grants:   []string{"oberth/data/release/cosign-secret"},
	}}

	results, err := syncGrantPolicies(context.Background(), store, "root", identities, "oberth-argo")
	if err != nil {
		t.Fatal(err)
	}

	// Expect: credentialed policy + ci-secrets policy + 4 per-repo items
	if len(results) < 6 {
		t.Fatalf("expected at least 6 results, got %d: %v", len(results), results)
	}

	// All items should be Changed since nothing existed (#459).
	for _, r := range results {
		if !r.Changed {
			t.Fatalf("item %q should be Changed=true for new provisioning, got false", r.Name)
		}
	}

	// Issue #456: the shared credentialed policy must NOT contain the grant
	// path when per-repo identities exist — it is stripped to zero stanzas.
	// The grant lives only in the per-repo policy.
	byCommand := runner.callsByCommand()
	call, ok := byCommand["policy write "+defaultCredentialedPolicy+" -"]
	if !ok {
		t.Fatal("credentialed policy write not called")
	}
	if strings.Contains(call.stdin, "cosign-secret") {
		t.Fatalf("shared credentialed policy must not contain grant paths when per-repo identities exist (issue #456):\n%s", call.stdin)
	}
	if strings.Contains(call.stdin, "upstream/") {
		t.Fatalf("shared credentialed policy must not contain upstream org stanzas when per-repo identities exist (issue #456):\n%s", call.stdin)
	}

	// The per-repo policy must still carry the grant.
	perRepoCall, ok := byCommand["policy write "+name+" -"]
	if !ok {
		t.Fatal("per-repo policy write not called")
	}
	if !strings.Contains(perRepoCall.stdin, "cosign-secret") {
		t.Fatalf("per-repo policy body should contain the grant path: %s", perRepoCall.stdin)
	}

	// The shared CI-secrets policy must also be zero-stanza.
	ciSecretsCall, ok := byCommand["policy write "+defaultCISecretsPolicy+" -"]
	if !ok {
		t.Fatal("ci-secrets policy write not called")
	}
	if strings.Contains(ciSecretsCall.stdin, "upstream/") {
		t.Fatalf("shared ci-secrets policy must not contain upstream org stanzas when per-repo identities exist (issue #456):\n%s", ciSecretsCall.stdin)
	}
}

func TestSyncGrantPoliciesIdempotent(t *testing.T) {
	t.Parallel()

	name := PerRepoName("codeberg", "oberthci", "oberth")
	ciName := PerRepoCIName("codeberg", "oberthci", "oberth")

	identities := []PerRepoIdentity{{
		Upstream: "codeberg",
		Org:      "oberthci",
		Repo:     "oberth",
	}}

	// Pre-compute the expected policies so the fake store returns them.
	// Issue #456: shared policies are always zero-stanza when per-repo
	// identities exist — no upstream org access, no grant paths.
	wantCredentialedPolicy := OberthCredentialedPolicyWithGrants(defaultKVPrefix, nil, nil)
	wantCISecretsPolicy := OberthCISecretsPolicy(defaultKVPrefix, nil)
	wantPerRepoPolicy := PerRepoPolicy(defaultKVPrefix, "oberthci", "oberth", nil)
	wantPerRepoCIPolicy := PerRepoCIPolicy(defaultKVPrefix, "oberthci", "oberth")

	matchingRoleJSON := func(n string) string {
		return `{"request_id":"1","data":{` +
			`"bound_service_account_names":["` + n + `"],` +
			`"bound_service_account_namespaces":["oberth-argo"],` +
			`"token_policies":["` + n + `"],` +
			`"token_no_default_policy":true,` +
			`"token_ttl":1200,` +
			`"token_max_ttl":1800}}`
	}

	responses := map[string]fakeBaoResponse{
		"policy read " + defaultCredentialedPolicy:         {out: wantCredentialedPolicy},
		"policy read " + defaultCISecretsPolicy:            {out: wantCISecretsPolicy},
		"policy read " + name:                              {out: wantPerRepoPolicy},
		"read -format=json auth/kubernetes/role/" + name:   {out: matchingRoleJSON(name)},
		"policy read " + ciName:                            {out: wantPerRepoCIPolicy},
		"read -format=json auth/kubernetes/role/" + ciName: {out: matchingRoleJSON(ciName)},
	}
	runner := &fakeBaoRunner{t: t, responses: responses}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	results, err := syncGrantPolicies(context.Background(), store, "root", identities, "oberth-argo")
	if err != nil {
		t.Fatal(err)
	}

	// Shared policies should be unchanged.
	if results[0].Name != "credentialed policy" || results[0].Changed {
		t.Fatalf("credentialed policy should be unchanged, got: %+v", results[0])
	}
	if results[1].Name != "ci-secrets policy" || results[1].Changed {
		t.Fatalf("ci-secrets policy should be unchanged, got: %+v", results[1])
	}

	// Per-repo items should also be unchanged (#459: Changed was previously
	// hardcoded to true, making sync output useless as a drift detector).
	for _, r := range results[2:] {
		if r.Changed {
			t.Fatalf("per-repo item %q should be unchanged in idempotent run, got Changed=true", r.Name)
		}
	}

	// No policy write or role write calls should exist — the runner would
	// fail on unscripted writes.
	byCommand := runner.callsByCommand()
	for cmd := range byCommand {
		if strings.HasPrefix(cmd, "policy write") || strings.HasPrefix(cmd, "write auth/") {
			t.Fatalf("unexpected write in idempotent run: %s", cmd)
		}
	}
}

// TestSyncGrantPoliciesSharedPoliciesZeroStanzaWithIdentities asserts the
// issue #456 fix: when per-repo identities exist, both shared policies are
// written with zero upstream org stanzas and zero approval-table grant paths.
// The shared identities are unselectable by admission once per-repo identities
// exist, so residual breadth is dormant standing privilege.
func TestSyncGrantPoliciesSharedPoliciesZeroStanzaWithIdentities(t *testing.T) {
	t.Parallel()

	name := PerRepoName("github", "skipops", "terraform")
	ciName := PerRepoCIName("github", "skipops", "terraform")

	responses := map[string]fakeBaoResponse{
		// Shared policies: currently carry the org-union (pre-#456 shape).
		// Sync must overwrite them with the zero-stanza shape.
		"policy read " + defaultCredentialedPolicy:         {out: OberthCredentialedPolicyWithGrants(defaultKVPrefix, []string{"skipops"}, []string{"release/cosign-secret"})},
		"policy write " + defaultCredentialedPolicy + " -": {out: "Success!"},
		"policy read " + defaultCISecretsPolicy:            {out: OberthCISecretsPolicy(defaultKVPrefix, []string{"skipops"})},
		"policy write " + defaultCISecretsPolicy + " -":    {out: "Success!"},
		// Per-repo identities: matching, so no write needed.
		"policy read " + name:                              {out: PerRepoPolicy(defaultKVPrefix, "skipops", "terraform", []string{"release/cosign-secret"})},
		"read -format=json auth/kubernetes/role/" + name:   {out: `{"request_id":"1","data":{"bound_service_account_names":["` + name + `"],"bound_service_account_namespaces":["oberth-argo"],"token_policies":["` + name + `"],"token_no_default_policy":true,"token_ttl":1200,"token_max_ttl":1800}}`},
		"policy read " + ciName:                            {out: PerRepoCIPolicy(defaultKVPrefix, "skipops", "terraform")},
		"read -format=json auth/kubernetes/role/" + ciName: {out: `{"request_id":"1","data":{"bound_service_account_names":["` + ciName + `"],"bound_service_account_namespaces":["oberth-argo"],"token_policies":["` + ciName + `"],"token_no_default_policy":true,"token_ttl":1200,"token_max_ttl":1800}}`},
	}
	runner := &fakeBaoRunner{t: t, responses: responses}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	identities := []PerRepoIdentity{{
		Upstream: "github",
		Org:      "skipops",
		Repo:     "terraform",
		Grants:   []string{"oberth/data/release/cosign-secret"},
	}}

	results, err := syncGrantPolicies(context.Background(), store, "root", identities, "oberth-argo")
	if err != nil {
		t.Fatal(err)
	}

	// Both shared policies must be Changed (overwritten from org-union to zero-stanza).
	if !results[0].Changed || results[0].Name != "credentialed policy" {
		t.Fatalf("credentialed policy should be Changed (strip to zero stanzas), got: %+v", results[0])
	}
	if !results[1].Changed || results[1].Name != "ci-secrets policy" {
		t.Fatalf("ci-secrets policy should be Changed (strip to zero stanzas), got: %+v", results[1])
	}

	// Verify the written shared policies contain no upstream or grant stanzas.
	byCommand := runner.callsByCommand()
	credCall := byCommand["policy write "+defaultCredentialedPolicy+" -"]
	if strings.Contains(credCall.stdin, "upstream/") {
		t.Fatalf("shared credentialed policy must not contain upstream paths:\n%s", credCall.stdin)
	}
	if strings.Contains(credCall.stdin, "cosign-secret") {
		t.Fatalf("shared credentialed policy must not contain grant paths:\n%s", credCall.stdin)
	}
	ciCall := byCommand["policy write "+defaultCISecretsPolicy+" -"]
	if strings.Contains(ciCall.stdin, "upstream/") {
		t.Fatalf("shared ci-secrets policy must not contain upstream paths:\n%s", ciCall.stdin)
	}

	// Per-repo items should be unchanged (already matching).
	for _, r := range results[2:] {
		if r.Changed {
			t.Fatalf("per-repo item %q should be unchanged, got Changed=true", r.Name)
		}
	}
}

func TestSyncGrantPoliciesNoIdentities(t *testing.T) {
	t.Parallel()

	// No identities -> both shared policies get "no upstream orgs" form (fail closed).
	wantCredentialedPolicy := OberthCredentialedPolicyWithGrants(defaultKVPrefix, nil, nil)
	wantCISecretsPolicy := OberthCISecretsPolicy(defaultKVPrefix, nil)

	responses := map[string]fakeBaoResponse{
		"policy read " + defaultCredentialedPolicy: {out: wantCredentialedPolicy},
		"policy read " + defaultCISecretsPolicy:    {out: wantCISecretsPolicy},
	}
	runner := &fakeBaoRunner{t: t, responses: responses}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	results, err := syncGrantPolicies(context.Background(), store, "root", nil, "oberth-argo")
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results (shared policies only), got %d", len(results))
	}
}

func TestSyncGrantPoliciesRejectsInvalidOrg(t *testing.T) {
	t.Parallel()

	identities := []PerRepoIdentity{{
		Upstream: "codeberg",
		Org:      "evil\"org",
		Repo:     "oberth",
	}}

	store := openBaoExec{run: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("should not be called")
	}, namespace: "openbao", pod: "openbao-0"}

	_, err := syncGrantPolicies(context.Background(), store, "root", identities, "oberth-argo")
	if err == nil {
		t.Fatal("expected error for invalid org name")
	}
	if !strings.Contains(err.Error(), "org name") {
		t.Fatalf("error should mention org name: %v", err)
	}
}
