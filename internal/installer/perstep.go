package installer

// Per-step Vault identities: when a secret grant names a specific step
// (template name) rather than the wildcard "*", the step receives its own
// ServiceAccount, Vault policy, and Vault role. This isolates the step's
// Vault credentials to exactly the paths granted to it, closing the residual
// gap where a per-repo union policy allowed any credentialed template to
// read any of the repo's granted paths at the Vault layer.
//
// Wildcard grants ("*") keep the per-repo identity (backward compatible).
// Steps without any grant keep the grant-free pipeline identity.
// Issue #623.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// PerStepIdentity describes one (repo, step) combination's per-step Vault
// identity. The installer creates the policy, role, and (via the chart) the
// ServiceAccount.
type PerStepIdentity struct {
	// Upstream is the registered upstream name (e.g. "codeberg", "github").
	Upstream string
	// Org is the upstream org identity.
	Org string
	// Repo is the repository's bare name.
	Repo string
	// Step is the Argo template name this identity is scoped to.
	Step string
	// GrantPaths are the paths this step is authorized to read. This includes
	// both paths with named grants for this step and paths with wildcard
	// grants for the same repo (inherited, since wildcard means "all
	// templates").
	GrantPaths []string
}

// perStepNamePrefix is the common prefix for all per-step Vault identities.
const perStepNamePrefix = "oberth-step-"

// PerStepName generates the deterministic, DNS-1123-safe name for a per-step
// identity. The name is shared across the ServiceAccount, the Vault policy,
// and the Vault role, because they are a single identity.
//
// The hash input is the full canonical quad (upstream/org/repo/step) so two
// steps in different repos that sanitise to the same readable portion get
// different names.
func PerStepName(upstream, org, repo, step string) string {
	canonical := upstream + "/" + org + "/" + repo + "/" + step
	digest := sha256.Sum256([]byte(canonical))
	hashSuffix := hex.EncodeToString(digest[:])[:12]

	var safe strings.Builder
	for _, c := range strings.ToLower(upstream + "-" + org + "-" + repo + "-" + step) {
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

	// Budget: prefix(12) + readable + "-" + hash(12) = total ≤ 63
	maxReadable := maxPerRepoNameLength - len(perStepNamePrefix) - 1 - len(hashSuffix)
	if len(readable) > maxReadable {
		readable = strings.TrimRight(readable[:maxReadable], "-")
	}
	if readable == "" {
		readable = "step"
	}
	return perStepNamePrefix + readable + "-" + hashSuffix
}

// PerStepPolicy generates the HCL policy for a single per-step identity. It
// grants:
//   - Read access to the repo's upstream org/repo namespace (same scope as
//     the per-repo policy — the step still needs upstream-scoped secrets)
//   - Exact-path read for only the paths granted to this step
//   - Token self-revocation
//
// Compared to the per-repo policy which carries ALL of a repo's grant paths,
// this carries only the paths relevant to one step, enforcing least privilege
// at the Vault layer.
func PerStepPolicy(kvPrefix, org, repo string, grantPaths []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, `# Per-step identity: read-only on this step's granted paths only.
# Managed by oberth secretstore sync. Do not edit manually. Issue #623.
path "%s/data/upstream/%s/%s/*" {
  capabilities = ["read"]
}`, kvPrefix, org, repo)

	seen := make(map[string]struct{}, len(grantPaths))
	for _, p := range grantPaths {
		if _, duplicate := seen[p]; duplicate {
			continue
		}
		seen[p] = struct{}{}
		fmt.Fprintf(&builder, "\n\n# Approved via the secret access table (per-step grant).\npath \"%s/data/%s\" {\n  capabilities = [\"read\"]\n}", kvPrefix, p)
	}

	builder.WriteString("\n\n# Allow the fetch client to revoke its own short-lived login token.\npath \"auth/token/revoke-self\" {\n  capabilities = [\"update\"]\n}")
	return builder.String() + denyServerIdentities(kvPrefix)
}

// perStepRoleMatches checks whether an existing Vault role matches the
// expected per-step shape: bound to the exact ServiceAccount name in the
// pipeline namespace, with the per-step policy and the standard TTLs.
func perStepRoleMatches(role map[string]any, saName, policyName, namespace string) bool {
	// Same shape check as perRepoRoleMatches — the convention is identical.
	return perRepoRoleMatches(role, saName, policyName, namespace)
}

// ConfigurePerStepIdentities creates or updates the Vault policy and role for
// each per-step identity. ServiceAccounts are managed by the Helm chart (via
// the perStepIdentities values list), not by this function.
//
// The function is idempotent: existing policies and roles that match the
// expected shape are left untouched; policies that drift are rewritten; roles
// with incompatible bindings fail loudly.
func ConfigurePerStepIdentities(ctx context.Context, store openBaoExec, rootToken string, identities []PerStepIdentity, argoNamespace string) ([]configItem, error) {
	var items []configItem

	for _, id := range identities {
		if err := ValidateOrgName(id.Org); err != nil {
			return items, fmt.Errorf("per-step identity org: %w", err)
		}
		if err := ValidateRepoName(id.Repo); err != nil {
			return items, fmt.Errorf("per-step identity repo: %w", err)
		}

		name := PerStepName(id.Upstream, id.Org, id.Repo, id.Step)

		// --- Policy ---

		grantPaths, err := credentialedPolicyPaths(defaultKVPrefix, id.GrantPaths)
		if err != nil {
			return items, fmt.Errorf("per-step %s: %w", name, err)
		}

		wantPolicy := PerStepPolicy(defaultKVPrefix, id.Org, id.Repo, grantPaths)
		havePolicy, policyExists, err := store.policyRead(ctx, rootToken, name)
		if err != nil {
			return items, fmt.Errorf("per-step policy read %s: %w", name, err)
		}
		policyChanged := !policyExists || strings.TrimSpace(havePolicy) != strings.TrimSpace(wantPolicy)
		if policyChanged {
			if err := store.policyWrite(ctx, rootToken, name, wantPolicy); err != nil {
				return items, fmt.Errorf("per-step policy write %s: %w", name, err)
			}
		}
		items = append(items, configItem{Name: "per-step policy " + name, Status: "✓", Changed: policyChanged})

		// --- Role ---

		rolePath := "auth/" + defaultAuthMount + "/role/" + name
		existingRole, err := store.readData(ctx, rootToken, rolePath)
		if err != nil {
			return items, fmt.Errorf("per-step role read %s: %w", name, err)
		}
		if existingRole != nil && !perStepRoleMatches(existingRole, name, name, argoNamespace) {
			return items, fmt.Errorf("per-step role %s exists with an unsafe or incompatible binding; refusing to overwrite it", name)
		}
		roleChanged := existingRole == nil
		if roleChanged {
			if err := store.writeJSON(ctx, rootToken, rolePath, map[string]any{
				"bound_service_account_names":      name,
				"bound_service_account_namespaces": argoNamespace,
				"token_policies":                   name,
				"token_no_default_policy":          true,
				"token_ttl":                        "20m",
				"token_max_ttl":                    "30m",
			}); err != nil {
				return items, fmt.Errorf("per-step role write %s: %w", name, err)
			}
		}
		items = append(items, configItem{Name: "per-step role " + name, Status: "✓", Changed: roleChanged})
	}

	return items, nil
}

// PerStepIdentityNames returns a sorted, deduplicated list of ServiceAccount
// names for all per-step identities. This is used by the installer to pass
// per-step SA names to the Helm chart.
func PerStepIdentityNames(identities []PerStepIdentity) []string {
	seen := make(map[string]struct{}, len(identities))
	var names []string
	for _, id := range identities {
		name := PerStepName(id.Upstream, id.Org, id.Repo, id.Step)
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// DerivePerStepIdentities computes per-step identities from per-repo
// identities and the full grant list. For each named-step grant (step != "*"),
// a per-step identity is produced with paths = the step's named grants PLUS
// any wildcard-granted paths for the same repo (inherited). Repos with only
// wildcard grants produce no per-step identities.
func DerivePerStepIdentities(repoIdentities []PerRepoIdentity, grants []grantWithStep) []PerStepIdentity {
	// Index grants by qualified repo key.
	type repoKey struct {
		upstream, org, repo string
	}
	wildcardPaths := make(map[repoKey][]string)        // paths with step="*"
	stepPaths := make(map[repoKey]map[string][]string) // step -> paths with named grants

	for _, g := range grants {
		key := repoKey{upstream: g.upstream, org: g.org, repo: g.repo}
		if g.step == "*" {
			wildcardPaths[key] = appendUnique(wildcardPaths[key], g.secret)
		} else {
			if stepPaths[key] == nil {
				stepPaths[key] = make(map[string][]string)
			}
			stepPaths[key][g.step] = appendUnique(stepPaths[key][g.step], g.secret)
		}
	}

	var result []PerStepIdentity
	for key, steps := range stepPaths {
		for step, paths := range steps {
			// Merge wildcard-granted paths (inherited by all steps).
			merged := make([]string, 0, len(paths)+len(wildcardPaths[key]))
			merged = append(merged, paths...)
			for _, wp := range wildcardPaths[key] {
				if !containsString(merged, wp) {
					merged = append(merged, wp)
				}
			}
			sort.Strings(merged)
			result = append(result, PerStepIdentity{
				Upstream:   key.upstream,
				Org:        key.org,
				Repo:       key.repo,
				Step:       step,
				GrantPaths: merged,
			})
		}
	}
	// Sort for deterministic output.
	sort.Slice(result, func(i, j int) bool {
		a := result[i].Upstream + "/" + result[i].Org + "/" + result[i].Repo + "/" + result[i].Step
		b := result[j].Upstream + "/" + result[j].Org + "/" + result[j].Repo + "/" + result[j].Step
		return a < b
	})
	return result
}

// grantWithStep is a fully resolved grant entry with separated upstream/org/repo
// and the step field. It is the input to DerivePerStepIdentities.
type grantWithStep struct {
	upstream, org, repo, step, secret string
}

func appendUnique(slice []string, val string) []string {
	for _, s := range slice {
		if s == val {
			return slice
		}
	}
	return append(slice, val)
}

func containsString(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}
