package installer

// Store login identities are scoped to an upstream repository, exact workflow
// file, and named or wildcard step. Wildcard inheritance stays within that file.
// Every granted main uses one of these identities; repository identities carry
// no explicit secret approvals.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
)

// PerStepIdentity describes one (repo, workflow file, step) combination's per-step OpenBao
// identity. The installer creates the policy, role, and (via the chart) the
// ServiceAccount.
type PerStepIdentity struct {
	// Upstream is the registered upstream name (e.g. "codeberg", "github").
	Upstream string
	// Org is the upstream org identity.
	Org string
	// Repo is the repository's bare name.
	Repo string
	// Step is the Argo template name, or * for this file's wildcard grants.
	Step string
	// Workflow is the exact repository-relative file.
	Workflow string
	// GrantPaths are the paths this step is authorized to read. This includes
	// both paths with named grants for this step and paths with wildcard
	// grants for the same repository and workflow file.
	GrantPaths []string
}

// PerStepName generates the deterministic, DNS-1123-safe name for a per-step
// identity. Delegates to internal/identityname so the plan and the installer
// always produce the same name (issue #623, finding 3).
func PerStepName(upstream, org, repo, workflow, step string) string {
	return identityname.PerStepName(upstream, org, repo, workflow, step)
}

// PerStepPolicy grants only exact approved paths and token self-revocation.
func PerStepPolicy(kvPrefix string, grantPaths []string) string {
	var builder strings.Builder
	builder.WriteString("# Per-step identity: exact granted paths only. Managed by Oberth.\n")

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

// perStepRoleMatches checks whether an existing OpenBao role matches the
// expected per-step shape: bound to the exact ServiceAccount name in the
// pipeline namespace, with the per-step policy and the standard TTLs.
func perStepRoleMatches(role map[string]any, saName, policyName, namespace string) bool {
	// Same shape check as perRepoRoleMatches — the convention is identical.
	return perRepoRoleMatches(role, saName, policyName, namespace)
}

// ConfigurePerStepIdentities creates or updates the OpenBao policy and role for
// each per-step identity. ServiceAccounts are managed by the Helm chart (via
// the perStepIdentities values list), not by this function.
//
// The function is idempotent: existing policies and roles that match the
// expected shape are left untouched; policies that drift are rewritten; roles
// with incompatible bindings fail loudly.
func ConfigurePerStepIdentities(ctx context.Context, store openBaoExec, rootToken string, identities []PerStepIdentity, argoNamespace string) ([]configItem, error) {
	items, err := configureStepTierIdentities(ctx, store, rootToken, identities, argoNamespace, false)
	if err != nil {
		return items, err
	}
	ci, err := configureStepTierIdentities(ctx, store, rootToken, identities, argoNamespace, true)
	return append(items, ci...), err
}
func configureStepTierIdentities(ctx context.Context, store openBaoExec, rootToken string, identities []PerStepIdentity, argoNamespace string, ci bool) ([]configItem, error) {
	var items []configItem

	for _, id := range identities {
		if err := ValidateOrgName(id.Org); err != nil {
			return items, fmt.Errorf("per-step identity org: %w", err)
		}
		if err := ValidateRepoName(id.Repo); err != nil {
			return items, fmt.Errorf("per-step identity repo: %w", err)
		}

		if err := model.ValidateWorkflowFile(id.Workflow); err != nil {
			return items, err
		}
		name := PerStepName(id.Upstream, id.Org, id.Repo, id.Workflow, id.Step)
		if ci {
			name = PerStepCIName(id.Upstream, id.Org, id.Repo, id.Workflow, id.Step)
		}

		// --- Policy ---

		grantPaths, err := credentialedPolicyPaths(defaultKVPrefix, id.GrantPaths)
		if err != nil {
			return items, fmt.Errorf("per-step %s: %w", name, err)
		}

		wantPolicy := PerStepPolicy(defaultKVPrefix, grantPaths)
		if ci {
			wantPolicy = PerStepCIPolicy(defaultKVPrefix, id.Org, id.Repo, grantPaths)
		}
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
		for _, name := range []string{PerStepName(id.Upstream, id.Org, id.Repo, id.Workflow, id.Step), PerStepCIName(id.Upstream, id.Org, id.Repo, id.Workflow, id.Step)} {
			if _, dup := seen[name]; !dup {
				seen[name] = struct{}{}
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// DerivePerStepIdentities computes identities for all scoped grants. Named
// steps inherit wildcard paths only from the same repository and workflow file.
// Wildcard grants have their own workflow-scoped identity.
func DerivePerStepIdentities(repoIdentities []PerRepoIdentity, grants []grantWithStep) []PerStepIdentity {
	// Index grants by qualified repo key.
	type repoKey struct {
		upstream, org, repo, workflow string
	}
	wildcardPaths := make(map[repoKey][]string)        // paths with step="*"
	stepPaths := make(map[repoKey]map[string][]string) // step -> paths with named grants

	for _, g := range grants {
		key := repoKey{upstream: g.upstream, org: g.org, repo: g.repo, workflow: g.workflow}
		if g.step == wildcardStep {
			wildcardPaths[key] = appendUnique(wildcardPaths[key], g.secret)
		} else {
			if stepPaths[key] == nil {
				stepPaths[key] = make(map[string][]string)
			}
			stepPaths[key][g.step] = appendUnique(stepPaths[key][g.step], g.secret)
		}
	}

	for key, paths := range wildcardPaths {
		if stepPaths[key] == nil {
			stepPaths[key] = map[string][]string{}
		}
		stepPaths[key]["*"] = paths
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
				Workflow:   key.workflow,
				Step:       step,
				GrantPaths: merged,
			})
		}
	}
	// Sort for deterministic output.
	sort.Slice(result, func(i, j int) bool {
		a := result[i].Upstream + "/" + result[i].Org + "/" + result[i].Repo + "/" + result[i].Workflow + "/" + result[i].Step
		b := result[j].Upstream + "/" + result[j].Org + "/" + result[j].Repo + "/" + result[j].Workflow + "/" + result[j].Step
		return a < b
	})
	return result
}

// grantWithStep is a fully resolved grant entry with separated upstream/org/repo
// and the step field. It is the input to DerivePerStepIdentities.
type grantWithStep struct {
	upstream, org, repo, step, secret, workflow string
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

// PerStepCIName is the ServiceAccount, role and policy for a CI grant key.
func PerStepCIName(upstream, org, repo, workflow, step string) string {
	return identityname.PerStepCIName(upstream, org, repo, workflow, step)
}

// PerStepCIPolicy limits a CI identity to exact granted upstream paths in its repository.
func PerStepCIPolicy(kvPrefix, org, repo string, paths []string) string {
	allowed := []string{}
	prefix := "upstream/" + org + "/" + repo + "/"
	for _, path := range paths {
		if strings.HasPrefix(path, prefix) {
			allowed = append(allowed, path)
		}
	}
	return PerStepPolicy(kvPrefix, allowed)
}
