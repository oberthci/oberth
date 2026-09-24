package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPerRepoNameIsDeterministic(t *testing.T) {
	t.Parallel()
	a := PerRepoName("codeberg", "oberthci", "oberth")
	b := PerRepoName("codeberg", "oberthci", "oberth")
	if a != b {
		t.Fatalf("PerRepoName not deterministic: %q != %q", a, b)
	}
}

func TestPerRepoNameDiffersForDifferentRepos(t *testing.T) {
	t.Parallel()
	a := PerRepoName("codeberg", "oberthci", "oberth")
	b := PerRepoName("codeberg", "oberthci", "cloudtaser-operator")
	if a == b {
		t.Fatalf("different repos produced the same name: %q", a)
	}
}

func TestPerRepoNameDiffersForDifferentUpstreams(t *testing.T) {
	t.Parallel()
	a := PerRepoName("codeberg", "oberthci", "oberth")
	b := PerRepoName("github", "oberthci", "oberth")
	if a == b {
		t.Fatalf("different upstreams produced the same name: %q", a)
	}
}

func TestPerRepoNameDiffersForDifferentOrgs(t *testing.T) {
	t.Parallel()
	a := PerRepoName("codeberg", "oberthci", "oberth")
	b := PerRepoName("codeberg", "skipops", "oberth")
	if a == b {
		t.Fatalf("different orgs produced the same name: %q", a)
	}
}

func TestPerRepoNameIsDNS1123Safe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		upstream, org, repo string
	}{
		{"codeberg", "oberthci", "oberth"},
		{"github", "skipops", "cloudtaser-operator"},
		{"my.upstream", "my.org", "my.repo"},
		{"UPPER", "CASE", "NAMES"},
		{"a", "b", "c"},
	}
	for _, tc := range cases {
		name := PerRepoName(tc.upstream, tc.org, tc.repo)
		if len(name) > maxPerRepoNameLength {
			t.Errorf("PerRepoName(%q,%q,%q) = %q (%d chars), exceeds limit %d",
				tc.upstream, tc.org, tc.repo, name, len(name), maxPerRepoNameLength)
		}
		// Must start and end with alphanumeric
		if name[0] < 'a' || name[0] > 'z' {
			t.Errorf("PerRepoName(%q,%q,%q) = %q, does not start with lowercase letter",
				tc.upstream, tc.org, tc.repo, name)
		}
		lastChar := name[len(name)-1]
		if (lastChar < 'a' || lastChar > 'z') && (lastChar < '0' || lastChar > '9') {
			t.Errorf("PerRepoName(%q,%q,%q) = %q, does not end with alphanumeric",
				tc.upstream, tc.org, tc.repo, name)
		}
		// Must contain only lowercase alphanumeric and hyphens
		for _, c := range name {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				t.Errorf("PerRepoName(%q,%q,%q) = %q, contains invalid character %q",
					tc.upstream, tc.org, tc.repo, name, string(c))
			}
		}
		// Must have the prefix
		if !strings.HasPrefix(name, perRepoNamePrefix) {
			t.Errorf("PerRepoName(%q,%q,%q) = %q, missing prefix %q",
				tc.upstream, tc.org, tc.repo, name, perRepoNamePrefix)
		}
	}
}

func TestPerRepoNameTruncatesLongNames(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 100)
	name := PerRepoName(long, long, long)
	if len(name) > maxPerRepoNameLength {
		t.Fatalf("PerRepoName with long inputs = %d chars, exceeds limit %d", len(name), maxPerRepoNameLength)
	}
}

func TestPerRepoPolicyContainsRepoScopedPath(t *testing.T) {
	t.Parallel()
	policy := PerRepoPolicy("oberth", "oberthci", "oberth", nil)
	if !strings.Contains(policy, `path "oberth/data/upstream/oberthci/oberth/*"`) {
		t.Fatalf("policy missing repo-scoped path:\n%s", policy)
	}
}

func TestPerRepoPolicyDoesNotContainAllUpstreamsWildcard(t *testing.T) {
	t.Parallel()
	policy := PerRepoPolicy("oberth", "oberthci", "oberth", nil)
	if strings.Contains(policy, `path "oberth/data/upstream/*"`) {
		t.Fatalf("per-repo policy must not contain the all-upstreams wildcard:\n%s", policy)
	}
}

func TestPerRepoPolicyContainsGrantPaths(t *testing.T) {
	t.Parallel()
	policy := PerRepoPolicy("oberth", "oberthci", "oberth", []string{"release/cosign-secret", "release/r2-token"})
	if !strings.Contains(policy, `path "oberth/data/release/cosign-secret"`) {
		t.Fatalf("policy missing grant path for cosign-secret:\n%s", policy)
	}
	if !strings.Contains(policy, `path "oberth/data/release/r2-token"`) {
		t.Fatalf("policy missing grant path for r2-token:\n%s", policy)
	}
}

func TestPerRepoPolicyDeduplicatesGrants(t *testing.T) {
	t.Parallel()
	policy := PerRepoPolicy("oberth", "oberthci", "oberth", []string{"release/cosign-secret", "release/cosign-secret"})
	count := strings.Count(policy, `path "oberth/data/release/cosign-secret"`)
	if count != 1 {
		t.Fatalf("duplicate grant path appeared %d times, want 1:\n%s", count, policy)
	}
}

func TestPerRepoPolicyContainsRevokeSelf(t *testing.T) {
	t.Parallel()
	policy := PerRepoPolicy("oberth", "oberthci", "oberth", nil)
	if !strings.Contains(policy, `path "auth/token/revoke-self"`) {
		t.Fatalf("policy missing revoke-self:\n%s", policy)
	}
}

func TestPerRepoPolicyCrossRepoReadRefused(t *testing.T) {
	t.Parallel()
	// A per-repo policy for org "oberthci" must not grant access to org "skipops"
	policy := PerRepoPolicy("oberth", "oberthci", "oberth", nil)
	if strings.Contains(policy, "skipops") {
		t.Fatalf("per-repo policy for oberthci must not reference skipops:\n%s", policy)
	}
	// The policy must not have the all-orgs wildcard
	if strings.Contains(policy, `"oberth/data/upstream/*"`) {
		t.Fatalf("per-repo policy must not contain cross-org wildcard:\n%s", policy)
	}
}

func TestPerRepoRoleMatchesCorrectShape(t *testing.T) {
	t.Parallel()
	role := map[string]any{
		"bound_service_account_names":      []any{"oberth-argo-codeberg-oberthci-oberth-abcdef012345"},
		"bound_service_account_namespaces": []any{"oberth-argo"},
		"token_policies":                   []any{"oberth-argo-codeberg-oberthci-oberth-abcdef012345"},
		"token_no_default_policy":          true,
		"token_ttl":                        float64(1200),
		"token_max_ttl":                    float64(1800),
	}
	if !perRepoRoleMatches(role, "oberth-argo-codeberg-oberthci-oberth-abcdef012345", "oberth-argo-codeberg-oberthci-oberth-abcdef012345", "oberth-argo") {
		t.Fatal("expected role to match")
	}
}

func TestPerRepoRoleRejectsWrongSA(t *testing.T) {
	t.Parallel()
	role := map[string]any{
		"bound_service_account_names":      []any{"wrong-sa"},
		"bound_service_account_namespaces": []any{"oberth-argo"},
		"token_policies":                   []any{"oberth-argo-codeberg-oberthci-oberth-abcdef012345"},
		"token_no_default_policy":          true,
		"token_ttl":                        float64(1200),
		"token_max_ttl":                    float64(1800),
	}
	if perRepoRoleMatches(role, "oberth-argo-codeberg-oberthci-oberth-abcdef012345", "oberth-argo-codeberg-oberthci-oberth-abcdef012345", "oberth-argo") {
		t.Fatal("role with wrong SA should not match")
	}
}

func TestConfigurePerRepoIdentitiesCreatesNewPolicyAndRole(t *testing.T) {
	t.Parallel()

	name := PerRepoName("codeberg", "oberthci", "oberth")
	ciName := PerRepoCIName("codeberg", "oberthci", "oberth")

	responses := map[string]fakeBaoResponse{
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
	}}

	items, err := ConfigurePerRepoIdentities(context.Background(), store, "root", identities, "oberth-argo")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 items (release policy + role + CI policy + role), got %d", len(items))
	}

	// Verify both release and CI policy writes were called
	byCommand := runner.callsByCommand()
	if _, ok := byCommand["policy write "+name+" -"]; !ok {
		t.Fatal("expected release policy write call")
	}
	if _, ok := byCommand["write auth/kubernetes/role/"+name+" -"]; !ok {
		t.Fatal("expected release role write call")
	}
	if _, ok := byCommand["policy write "+ciName+" -"]; !ok {
		t.Fatal("expected CI policy write call")
	}
	if _, ok := byCommand["write auth/kubernetes/role/"+ciName+" -"]; !ok {
		t.Fatal("expected CI role write call")
	}
}

func TestConfigurePerRepoIdentitiesSkipsExistingMatchingRole(t *testing.T) {
	t.Parallel()

	name := PerRepoName("codeberg", "oberthci", "oberth")
	ciName := PerRepoCIName("codeberg", "oberthci", "oberth")
	wantPolicy := PerRepoPolicy(defaultKVPrefix, "oberthci", "oberth", nil)
	wantCIPolicy := PerRepoCIPolicy(defaultKVPrefix, "oberthci", "oberth")

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
		"policy read " + name:                            {out: wantPolicy},
		"read -format=json auth/kubernetes/role/" + name: {out: matchingRoleJSON(name)},
		// CI identity also exists and matches
		"policy read " + ciName:                            {out: wantCIPolicy},
		"read -format=json auth/kubernetes/role/" + ciName: {out: matchingRoleJSON(ciName)},
	}
	runner := &fakeBaoRunner{t: t, responses: responses}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	identities := []PerRepoIdentity{{
		Upstream: "codeberg",
		Org:      "oberthci",
		Repo:     "oberth",
	}}

	items, err := ConfigurePerRepoIdentities(context.Background(), store, "root", identities, "oberth-argo")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 items (release + CI, policy + role each), got %d", len(items))
	}

	// No write calls should have been made
	byCommand := runner.callsByCommand()
	for cmd := range byCommand {
		if strings.HasPrefix(cmd, "policy write") || strings.HasPrefix(cmd, "write auth/") {
			t.Fatalf("unexpected write call: %s", cmd)
		}
	}
}

func TestConfigurePerRepoIdentitiesWithGrants(t *testing.T) {
	t.Parallel()

	name := PerRepoName("codeberg", "oberthci", "oberth")
	ciName := PerRepoCIName("codeberg", "oberthci", "oberth")

	responses := map[string]fakeBaoResponse{
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

	items, err := ConfigurePerRepoIdentities(context.Background(), store, "root", identities, "oberth-argo")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 items (release + CI, policy + role each), got %d", len(items))
	}

	// Verify the release policy body contains the grant path
	for _, call := range runner.calls {
		if call.command == "policy write "+name+" -" {
			if !strings.Contains(call.stdin, "release/cosign-secret") {
				t.Fatalf("release policy body missing grant path:\n%s", call.stdin)
			}
		}
		// Verify the CI policy body does NOT contain the grant path
		if call.command == "policy write "+ciName+" -" {
			if strings.Contains(call.stdin, "release/cosign-secret") {
				t.Fatalf("CI policy body must not contain release grant paths:\n%s", call.stdin)
			}
		}
	}
}

func TestPerRepoIdentityNamesReturnsSorted(t *testing.T) {
	t.Parallel()
	ids := []PerRepoIdentity{
		{Upstream: "github", Org: "skipops", Repo: "terraform"},
		{Upstream: "codeberg", Org: "oberthci", Repo: "oberth"},
		{Upstream: "codeberg", Org: "oberthci", Repo: "cloudtaser-operator"},
	}
	names := PerRepoIdentityNames(ids)
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Fatalf("names not sorted: %v", names)
		}
	}
}

func TestPerRepoIdentityNamesDeduplicates(t *testing.T) {
	t.Parallel()
	ids := []PerRepoIdentity{
		{Upstream: "codeberg", Org: "oberthci", Repo: "oberth"},
		{Upstream: "codeberg", Org: "oberthci", Repo: "oberth"},
	}
	names := PerRepoIdentityNames(ids)
	if len(names) != 1 {
		t.Fatalf("expected 1 unique name, got %d: %v", len(names), names)
	}
}

// --- Per-repo CI identity tests (issue #433) ---

func TestPerRepoCINameIsDeterministic(t *testing.T) {
	t.Parallel()
	a := PerRepoCIName("codeberg", "oberthci", "oberth")
	b := PerRepoCIName("codeberg", "oberthci", "oberth")
	if a != b {
		t.Fatalf("PerRepoCIName not deterministic: %q != %q", a, b)
	}
}

func TestPerRepoCINameDiffersFromReleaseName(t *testing.T) {
	t.Parallel()
	release := PerRepoName("codeberg", "oberthci", "oberth")
	ci := PerRepoCIName("codeberg", "oberthci", "oberth")
	if release == ci {
		t.Fatalf("CI and release names must differ, both are %q", release)
	}
}

func TestPerRepoCINameHasCIPrefix(t *testing.T) {
	t.Parallel()
	name := PerRepoCIName("codeberg", "oberthci", "oberth")
	if !strings.HasPrefix(name, "oberth-argo-ci-") {
		t.Fatalf("CI name %q missing oberth-argo-ci- prefix", name)
	}
}

func TestPerRepoCIPolicyScopedToRepoNamespace(t *testing.T) {
	t.Parallel()
	policy := PerRepoCIPolicy("oberth", "oberthci", "oberth")
	if !strings.Contains(policy, `path "oberth/data/upstream/oberthci/oberth/*"`) {
		t.Fatalf("CI policy missing repo-scoped path:\n%s", policy)
	}
}

func TestPerRepoCIPolicyHasNoGrants(t *testing.T) {
	t.Parallel()
	policy := PerRepoCIPolicy("oberth", "oberthci", "oberth")
	if strings.Contains(policy, "Approved via the secret access table") {
		t.Fatalf("CI policy must not contain approval-table grants:\n%s", policy)
	}
	if strings.Contains(policy, "release/") {
		t.Fatalf("CI policy must not reference release paths:\n%s", policy)
	}
}

func TestPerRepoCIPolicyNoCrossOrgAccess(t *testing.T) {
	t.Parallel()
	policy := PerRepoCIPolicy("oberth", "oberthci", "oberth")
	if strings.Contains(policy, "skipops") {
		t.Fatalf("CI policy for oberthci/oberth must not reference skipops:\n%s", policy)
	}
	if strings.Contains(policy, `"oberth/data/upstream/*"`) {
		t.Fatalf("CI policy must not contain cross-org wildcard:\n%s", policy)
	}
}

func TestPerRepoCIIdentityNamesReturnsSorted(t *testing.T) {
	t.Parallel()
	ids := []PerRepoIdentity{
		{Upstream: "github", Org: "skipops", Repo: "terraform"},
		{Upstream: "codeberg", Org: "oberthci", Repo: "oberth"},
	}
	names := PerRepoCIIdentityNames(ids)
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Fatalf("names not sorted: %v", names)
		}
	}
}

// --- Per-repo identity repo name validation tests (issue #416) ---

func TestConfigurePerRepoIdentitiesRejectsHCLInjectionInRepo(t *testing.T) {
	t.Parallel()

	responses := map[string]fakeBaoResponse{}
	runner := &fakeBaoRunner{t: t, responses: responses}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	identities := []PerRepoIdentity{{
		Upstream: "codeberg",
		Org:      "oberthci",
		Repo:     `evil" { capabilities = ["sudo"] } path "secret/*`,
	}}

	_, err := ConfigurePerRepoIdentities(context.Background(), store, "root", identities, "oberth-argo")
	if err == nil {
		t.Fatal("expected rejection of repo name with HCL injection characters")
	}
	if !strings.Contains(err.Error(), "characters outside") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestConfigurePerRepoIdentitiesRejectsControlCharsInRepo(t *testing.T) {
	t.Parallel()

	responses := map[string]fakeBaoResponse{}
	runner := &fakeBaoRunner{t: t, responses: responses}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	for _, repo := range []string{"repo\x00evil", "repo\nevil", "repo\tevil"} {
		identities := []PerRepoIdentity{{
			Upstream: "codeberg",
			Org:      "oberthci",
			Repo:     repo,
		}}
		if _, err := ConfigurePerRepoIdentities(context.Background(), store, "root", identities, "oberth-argo"); err == nil {
			t.Errorf("ConfigurePerRepoIdentities(repo=%q) = nil; want error for control character", repo)
		}
	}
}

func TestConfigurePerRepoIdentitiesRejectsHCLInjectionInOrg(t *testing.T) {
	t.Parallel()

	responses := map[string]fakeBaoResponse{}
	runner := &fakeBaoRunner{t: t, responses: responses}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	identities := []PerRepoIdentity{{
		Upstream: "codeberg",
		Org:      `evil" { capabilities = ["sudo"] }`,
		Repo:     "oberth",
	}}

	_, err := ConfigurePerRepoIdentities(context.Background(), store, "root", identities, "oberth-argo")
	if err == nil {
		t.Fatal("expected rejection of org name with HCL injection characters")
	}
	if !strings.Contains(err.Error(), "characters outside") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestGrantlessRepoGetsNoPerRepoSA(t *testing.T) {
	t.Parallel()
	// A repo with no grants and no declared secret-store paths should not
	// appear in the per-repo identities at all. This test verifies the policy
	// generation still works (produces an org-scoped-only policy) but the
	// caller's responsibility is to not include grantless repos.
	policy := PerRepoPolicy("oberth", "oberthci", "oberth", nil)
	// Only the org wildcard and revoke-self should be present
	lines := strings.Split(policy, "\n")
	pathCount := 0
	for _, line := range lines {
		if strings.Contains(line, `path "`) {
			pathCount++
		}
	}
	if pathCount != 2 { // org wildcard + revoke-self
		t.Fatalf("grantless policy should have exactly 2 path entries, got %d:\n%s", pathCount, policy)
	}
}
