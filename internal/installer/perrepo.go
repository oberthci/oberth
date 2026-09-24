package installer

// Per-repo Vault identities: each secret-declaring repository gets its own
// ServiceAccount, Vault policy, and Vault role. This replaces the shared
// tier-wide credentialed/ci-secrets identities with per-repo boundaries so
// one repository cannot reach another's secrets at the Vault layer.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// PerRepoIdentity describes one repository's per-repo Vault identity. The
// installer creates the policy, role, and (via the chart) the ServiceAccount.
type PerRepoIdentity struct {
	// Upstream is the registered upstream name (e.g. "codeberg", "github").
	Upstream string
	// Org is the upstream org identity, the trailing segment of the base URL
	// (e.g. "oberthci", "skipops").
	Org string
	// Repo is the repository's bare name (e.g. "oberth", "cloudtaser-operator").
	Repo string
	// Grants are the full KV data paths this repo has active approval-table
	// grants for. Each is a path like "oberth/data/release/cosign-secret".
	// Only release-tier repos need grants; CI-tier repos get org-scoped
	// upstream access only.
	Grants []string
}

// perRepoNamePrefix is the common prefix for all per-repo release Vault identities.
const perRepoNamePrefix = "oberth-argo-"

// perRepoCINamePrefix is the common prefix for all per-repo CI Vault identities.
// It is distinct from perRepoNamePrefix so the CI and release SAs, policies, and
// roles are always structurally different names — even for the same repository.
const perRepoCINamePrefix = "oberth-argo-ci-"

// maxPerRepoNameLength caps the generated name so it fits comfortably within
// the Kubernetes DNS-1123 subdomain limit (253) and the label value limit (63).
// The real constraint is the label value on the chart's ServiceAccount: 63
// characters. With the prefix "oberth-argo-" (12) and the hash suffix "-<12>"
// (13), that leaves 38 for the readable portion.
const maxPerRepoNameLength = 63

// PerRepoName generates the deterministic, DNS-1123-safe name for a per-repo
// identity. The name is shared across the ServiceAccount, the Vault policy,
// and the Vault role, because they are a single identity.
//
// The readable portion is derived from upstream-org-repo, and a truncation-
// safe hash suffix ensures uniqueness even when the readable portion is
// clipped.
func PerRepoName(upstream, org, repo string) string {
	// The canonical input for the hash is the unmodified triple, so two repos
	// that sanitise to the same readable prefix get different names.
	canonical := upstream + "/" + org + "/" + repo
	digest := sha256.Sum256([]byte(canonical))
	hashSuffix := hex.EncodeToString(digest[:])[:12]

	var safe strings.Builder
	for _, c := range strings.ToLower(upstream + "-" + org + "-" + repo) {
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-':
			safe.WriteRune(c)
		default:
			safe.WriteByte('-')
		}
	}
	readable := strings.Trim(safe.String(), "-")

	// Budget: prefix(12) + readable + "-" + hash(12) = total
	maxReadable := maxPerRepoNameLength - len(perRepoNamePrefix) - 1 - len(hashSuffix)
	if len(readable) > maxReadable {
		readable = strings.TrimRight(readable[:maxReadable], "-")
	}
	if readable == "" {
		readable = "repo"
	}
	return perRepoNamePrefix + readable + "-" + hashSuffix
}

// PerRepoCIName generates the deterministic, DNS-1123-safe name for a per-repo
// CI-tier identity. It follows the same scheme as PerRepoName but uses the
// perRepoCINamePrefix, producing a structurally distinct name. The CI and
// release identities for the same repository are always different names.
func PerRepoCIName(upstream, org, repo string) string {
	canonical := upstream + "/" + org + "/" + repo
	digest := sha256.Sum256([]byte(canonical))
	hashSuffix := hex.EncodeToString(digest[:])[:12]

	var safe strings.Builder
	for _, c := range strings.ToLower(upstream + "-" + org + "-" + repo) {
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-':
			safe.WriteRune(c)
		default:
			safe.WriteByte('-')
		}
	}
	readable := strings.Trim(safe.String(), "-")

	maxReadable := maxPerRepoNameLength - len(perRepoCINamePrefix) - 1 - len(hashSuffix)
	if len(readable) > maxReadable {
		readable = strings.TrimRight(readable[:maxReadable], "-")
	}
	if readable == "" {
		readable = "repo"
	}
	return perRepoCINamePrefix + readable + "-" + hashSuffix
}

// PerRepoCIPolicy generates the HCL policy for a single repository's per-repo
// CI-tier identity. It grants:
//   - Read access to the repo's upstream org/repo namespace only
//   - Token self-revocation
//
// Unlike the release-tier PerRepoPolicy, it NEVER carries approval-table grants.
// This is the structural guarantee that a CI run cannot reach release secrets
// even if a CI per-repo identity is used. The policy is scoped to
// path "oberth/data/upstream/<org>/<repo>/*" — the same subtree the release
// policy uses, but without any exact-path grant entries.
func PerRepoCIPolicy(kvPrefix, org, repo string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, `# Per-repo CI identity: read-only on this repo's upstream namespace.
# CI-tier policy: no approval-table grants — release secrets are unreachable.
# Managed by oberth install --install-secretstore. Do not edit manually.
path "%s/data/upstream/%s/%s/*" {
  capabilities = ["read"]
}`, kvPrefix, org, repo)

	builder.WriteString("\n\n# Allow the fetch client to revoke its own short-lived login token.\npath \"auth/token/revoke-self\" {\n  capabilities = [\"update\"]\n}")
	return builder.String()
}

// PerRepoPolicy generates the HCL policy for a single repository's per-repo
// identity. It grants:
//   - Read access to the repo's upstream org namespace (all secrets shared
//     within the org)
//   - Any exact-path release grants from the approval table for this specific
//     repo
//   - Token self-revocation
//
// Compared to the shared credentialed policy which has path "oberth/data/upstream/*"
// (all orgs), this scopes to the exact repo's namespace only:
// path "oberth/data/upstream/<org>/<repo>/*". The grant paths from the
// secret access table are added as exact-path rules for release-tier
// credentials.
func PerRepoPolicy(kvPrefix, org, repo string, grantPaths []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, `# Per-repo identity: read-only on this repo's namespace.
# Managed by oberth install --install-secretstore. Do not edit manually.
path "%s/data/upstream/%s/%s/*" {
  capabilities = ["read"]
}`, kvPrefix, org, repo)

	seen := make(map[string]struct{}, len(grantPaths))
	for _, p := range grantPaths {
		if _, duplicate := seen[p]; duplicate {
			continue
		}
		seen[p] = struct{}{}
		fmt.Fprintf(&builder, "\n\n# Approved via the secret access table.\npath \"%s/data/%s\" {\n  capabilities = [\"read\"]\n}", kvPrefix, p)
	}

	builder.WriteString("\n\n# Allow the fetch client to revoke its own short-lived login token.\npath \"auth/token/revoke-self\" {\n  capabilities = [\"update\"]\n}")
	return builder.String()
}

// perRepoRoleMatches checks whether an existing Vault role matches the expected
// per-repo shape: bound to the exact ServiceAccount name in the pipeline
// namespace, with the per-repo policy and the standard TTLs.
func perRepoRoleMatches(role map[string]any, saName, policyName, namespace string) bool {
	noDefaultPolicy, noDefaultPolicyOK := role["token_no_default_policy"].(bool)
	tokenTTL, tokenTTLOK := role["token_ttl"].(float64)
	tokenMaxTTL, tokenMaxTTLOK := role["token_max_ttl"].(float64)
	return exactSingletonString(role["bound_service_account_names"], saName) &&
		exactSingletonString(role["bound_service_account_namespaces"], namespace) &&
		exactSingletonString(role["token_policies"], policyName) &&
		noDefaultPolicyOK && noDefaultPolicy &&
		tokenTTLOK && tokenTTL == 1200 &&
		tokenMaxTTLOK && tokenMaxTTL == 1800
}

// ConfigurePerRepoIdentities creates or updates the Vault policy and role for
// each per-repo identity. ServiceAccounts are managed by the Helm chart (see
// argo-identities.yaml), not by this function.
//
// The function is idempotent: existing policies and roles that match the
// expected shape are left untouched; policies that drift are rewritten (the
// approval-table-derived grants may have changed); roles with incompatible
// bindings fail loudly rather than being silently overwritten.
func ConfigurePerRepoIdentities(ctx context.Context, store openBaoExec, rootToken string, identities []PerRepoIdentity, argoNamespace string) ([]configItem, error) {
	var items []configItem

	for _, id := range identities {
		// Validate org and repo before any HCL interpolation. The org is also
		// validated upstream for shared policies, but defense-in-depth here
		// catches per-repo-only paths. The repo field had no validation at all
		// prior to issue #416.
		if err := ValidateOrgName(id.Org); err != nil {
			return items, fmt.Errorf("per-repo identity org: %w", err)
		}
		if err := ValidateRepoName(id.Repo); err != nil {
			return items, fmt.Errorf("per-repo identity repo: %w", err)
		}

		name := PerRepoName(id.Upstream, id.Org, id.Repo)

		// --- Policy ---

		grantPaths, err := credentialedPolicyPaths(defaultKVPrefix, id.Grants)
		if err != nil {
			return items, fmt.Errorf("per-repo %s: %w", name, err)
		}

		wantPolicy := PerRepoPolicy(defaultKVPrefix, id.Org, id.Repo, grantPaths)
		havePolicy, policyExists, err := store.policyRead(ctx, rootToken, name)
		if err != nil {
			return items, fmt.Errorf("per-repo policy read %s: %w", name, err)
		}
		if !policyExists || strings.TrimSpace(havePolicy) != strings.TrimSpace(wantPolicy) {
			if err := store.policyWrite(ctx, rootToken, name, wantPolicy); err != nil {
				return items, fmt.Errorf("per-repo policy write %s: %w", name, err)
			}
		}
		items = append(items, configItem{Name: "per-repo policy " + name, Status: "✓"})

		// --- Role ---

		rolePath := "auth/" + defaultAuthMount + "/role/" + name
		existingRole, err := store.readData(ctx, rootToken, rolePath)
		if err != nil {
			return items, fmt.Errorf("per-repo role read %s: %w", name, err)
		}
		if existingRole != nil && !perRepoRoleMatches(existingRole, name, name, argoNamespace) {
			return items, fmt.Errorf("per-repo role %s exists with an unsafe or incompatible binding; refusing to overwrite it", name)
		}
		if existingRole == nil {
			if err := store.writeJSON(ctx, rootToken, rolePath, map[string]any{
				"bound_service_account_names":      name,
				"bound_service_account_namespaces": argoNamespace,
				"token_policies":                   name,
				"token_no_default_policy":          true,
				"token_ttl":                        "20m",
				"token_max_ttl":                    "30m",
			}); err != nil {
				return items, fmt.Errorf("per-repo role write %s: %w", name, err)
			}
		}
		items = append(items, configItem{Name: "per-repo role " + name, Status: "✓"})

		// --- CI-tier per-repo identity ---
		// Every repo with a release per-repo identity also gets a CI per-repo
		// identity. The CI policy is grant-free: it scopes upstream access to
		// this repo's own namespace only, closing the org-union gap where the
		// shared ci-secrets policy gave every CI run read access to every
		// registered org's upstream subtree (issue #433).

		ciName := PerRepoCIName(id.Upstream, id.Org, id.Repo)

		wantCIPolicy := PerRepoCIPolicy(defaultKVPrefix, id.Org, id.Repo)
		haveCIPolicy, ciPolicyExists, err := store.policyRead(ctx, rootToken, ciName)
		if err != nil {
			return items, fmt.Errorf("per-repo CI policy read %s: %w", ciName, err)
		}
		if !ciPolicyExists || strings.TrimSpace(haveCIPolicy) != strings.TrimSpace(wantCIPolicy) {
			if err := store.policyWrite(ctx, rootToken, ciName, wantCIPolicy); err != nil {
				return items, fmt.Errorf("per-repo CI policy write %s: %w", ciName, err)
			}
		}
		items = append(items, configItem{Name: "per-repo ci policy " + ciName, Status: "✓"})

		ciRolePath := "auth/" + defaultAuthMount + "/role/" + ciName
		existingCIRole, err := store.readData(ctx, rootToken, ciRolePath)
		if err != nil {
			return items, fmt.Errorf("per-repo CI role read %s: %w", ciName, err)
		}
		if existingCIRole != nil && !perRepoRoleMatches(existingCIRole, ciName, ciName, argoNamespace) {
			return items, fmt.Errorf("per-repo CI role %s exists with an unsafe or incompatible binding; refusing to overwrite it", ciName)
		}
		if existingCIRole == nil {
			if err := store.writeJSON(ctx, rootToken, ciRolePath, map[string]any{
				"bound_service_account_names":      ciName,
				"bound_service_account_namespaces": argoNamespace,
				"token_policies":                   ciName,
				"token_no_default_policy":          true,
				"token_ttl":                        "20m",
				"token_max_ttl":                    "30m",
			}); err != nil {
				return items, fmt.Errorf("per-repo CI role write %s: %w", ciName, err)
			}
		}
		items = append(items, configItem{Name: "per-repo ci role " + ciName, Status: "✓"})
	}

	return items, nil
}

// PerRepoIdentityNames returns a sorted, deduplicated list of ServiceAccount
// names for all per-repo identities. This is used by the installer to pass
// per-repo SA names to the Helm chart.
func PerRepoIdentityNames(identities []PerRepoIdentity) []string {
	seen := make(map[string]struct{}, len(identities))
	var names []string
	for _, id := range identities {
		name := PerRepoName(id.Upstream, id.Org, id.Repo)
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// PerRepoCIIdentityNames returns a sorted, deduplicated list of ServiceAccount
// names for all per-repo CI identities. This is used by the installer to pass
// per-repo CI SA names to the Helm chart.
func PerRepoCIIdentityNames(identities []PerRepoIdentity) []string {
	seen := make(map[string]struct{}, len(identities))
	var names []string
	for _, id := range identities {
		name := PerRepoCIName(id.Upstream, id.Org, id.Repo)
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// upstreamOrgsFromIdentities extracts a sorted, deduplicated list of upstream
// org names from the per-repo identity set. This is used by the shared
// credentialed and CI-secrets policies to enumerate exactly the registered
// upstream subtrees instead of a static "upstream/*" wildcard (issue #246
// design revision). Repos with active grants always have per-repo identities,
// so their orgs are present; a fresh install with no identities produces an
// empty list — fail closed.
func upstreamOrgsFromIdentities(identities []PerRepoIdentity) []string {
	seen := make(map[string]struct{}, len(identities))
	var orgs []string
	for _, id := range identities {
		if id.Org == "" {
			continue
		}
		if _, dup := seen[id.Org]; !dup {
			seen[id.Org] = struct{}{}
			orgs = append(orgs, id.Org)
		}
	}
	sort.Strings(orgs)
	return orgs
}
