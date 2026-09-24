package argojob

// Per-repo identity selection: when a repository has a per-repo Vault
// identity (SA + policy + role), the RELEASE-trigger submission path
// selects it instead of the shared credentialed identity, scoping the
// repo's Vault access to its own namespace and its own approved grants.
// CI (branch) triggers always keep the shared grant-free ci-secrets
// identity — see identityForWithRepo for the #200 boundary this preserves.

import (
	"fmt"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// PerRepoIdentityConfig describes a repository's per-repo Vault identity.
// The ServiceAccount name doubles as the Vault role name and policy name.
type PerRepoIdentityConfig struct {
	// ServiceAccountName is the per-repo SA in the pipeline namespace.
	// It is also the Vault role name and policy name.
	ServiceAccountName string
}

// canonicalRepoKey constructs the map key for PerRepoIdentities from
// the upstream name, org, and bare repo name. This is the same canonical
// "upstream/org/repo" form used for grant storage (#245 BLOCKER B).
func canonicalRepoKey(upstreamName, org, repo string) string {
	if upstreamName == "" || org == "" {
		return repo
	}
	return upstreamName + "/" + org + "/" + repo
}

// identityForPerRepo selects the per-repo identity for a credentialed run.
// It returns the per-repo identity if one exists for this repo; otherwise
// it returns false and the caller falls back to the shared tier identity.
func (config Config) identityForPerRepo(upstreamName, org, repo string) (argoworkflow.Identity, bool) {
	if config.PerRepoIdentities == nil {
		return argoworkflow.Identity{}, false
	}
	key := canonicalRepoKey(upstreamName, org, repo)
	perRepo, exists := config.PerRepoIdentities[key]
	if !exists || perRepo.ServiceAccountName == "" {
		return argoworkflow.Identity{}, false
	}
	return argoworkflow.Identity{
		Namespace:                    config.Namespace,
		ServiceAccountName:           perRepo.ServiceAccountName,
		ExecutorServiceAccountName:   config.ExecutorServiceAccount,
		AutomountServiceAccountToken: true,
	}, true
}

// vaultRoleForPerRepo returns the per-repo Vault role name if one exists.
// The per-repo SA name IS the role name (they share the same name by
// convention). Returns empty string if no per-repo identity exists.
func (config Config) vaultRoleForPerRepo(upstreamName, org, repo string) string {
	if config.PerRepoIdentities == nil {
		return ""
	}
	key := canonicalRepoKey(upstreamName, org, repo)
	perRepo, exists := config.PerRepoIdentities[key]
	if !exists {
		return ""
	}
	return perRepo.ServiceAccountName
}

// identityForPerRepoCI selects the per-repo CI identity for a CI run.
// It returns the CI per-repo identity if one exists for this repo; otherwise
// it returns false and the caller falls back to the shared ci-secrets identity.
func (config Config) identityForPerRepoCI(upstreamName, org, repo string) (argoworkflow.Identity, bool) {
	if config.PerRepoCIIdentities == nil {
		return argoworkflow.Identity{}, false
	}
	key := canonicalRepoKey(upstreamName, org, repo)
	perRepo, exists := config.PerRepoCIIdentities[key]
	if !exists || perRepo.ServiceAccountName == "" {
		return argoworkflow.Identity{}, false
	}
	return argoworkflow.Identity{
		Namespace:                    config.Namespace,
		ServiceAccountName:           perRepo.ServiceAccountName,
		ExecutorServiceAccountName:   config.ExecutorServiceAccount,
		AutomountServiceAccountToken: true,
	}, true
}

// vaultRoleForPerRepoCI returns the per-repo CI Vault role name if one exists.
// The CI per-repo SA name IS the role name (same convention as the release
// tier). Returns empty string if no CI per-repo identity exists.
func (config Config) vaultRoleForPerRepoCI(upstreamName, org, repo string) string {
	if config.PerRepoCIIdentities == nil {
		return ""
	}
	key := canonicalRepoKey(upstreamName, org, repo)
	perRepo, exists := config.PerRepoCIIdentities[key]
	if !exists {
		return ""
	}
	return perRepo.ServiceAccountName
}

// identityForWithRepo selects the ServiceAccount for a run, consulting
// per-repo identities for both the RELEASE and CI triggers.
//
// Release trigger: uses the release per-repo identity when one exists. When
// other repos have per-repo identities but the triggering repo does not, the
// run is refused — falling back to the shared credentialed SA would leak
// cross-repo secrets because its policy folds all repos' grants (issue #434).
//
// CI trigger: uses the CI per-repo identity when one exists. The CI per-repo
// policy is structurally grant-free — it scopes upstream access to the repo's
// own namespace only (issue #433). When other repos have CI per-repo
// identities but the triggering repo does not, the run is refused — falling
// back to the shared ci-secrets SA would leak cross-org upstream secrets
// because its policy is an org-union of all registered orgs (issue #433).
// The CI per-repo identity NEVER carries release-tier grants, preserving the
// CI-to-release boundary (issue #200).
//
// For fragment runs, the caller must pass the HOST repo name (the repo that
// declared the pipeline), not the fragment source's repo. This ensures the
// fragment runs under the host's identity, because the host's pipeline is
// what declared the secrets and the host's approval table is what was checked.
func (config Config) identityForWithRepo(trigger periapsis.Trigger, hasSecretPaths bool, upstreamName, org, repo string) (argoworkflow.Identity, error) {
	if !hasSecretPaths {
		// No secrets: always the pipeline SA, regardless of per-repo config.
		return config.identityFor(trigger, false)
	}

	if trigger == periapsis.TriggerRelease {
		if identity, ok := config.identityForPerRepo(upstreamName, org, repo); ok {
			return identity, nil
		}
		// The shared credentialed SA's Vault policy
		// (OberthCredentialedPolicyWithGrants) folds approved paths from
		// every repo into one policy. When other repos already have
		// per-repo identities — meaning the approval table holds grants
		// for more than one repo — falling back to the shared SA would
		// give this repo's release run read access to every other repo's
		// secrets. Refuse admission and direct the operator to provision
		// a per-repo identity. The single-repo case (no other per-repo
		// identities) is safe: the shared policy carries only this repo's
		// grants. (Issue #434)
		if len(config.PerRepoIdentities) > 0 {
			return argoworkflow.Identity{}, fmt.Errorf(
				"argojob: refusing release submission for %q: %d other repo(s) hold per-repo "+
					"identities with their own secret grants, and this repo has no per-repo identity; "+
					"the shared credentialed service account's Vault policy includes all repos' grants, "+
					"so falling back to it would leak cross-repo secrets; "+
					"run \"oberth secretstore sync\" (or \"oberth install --install-secretstore --upgrade\") to provision per-repo identities",
				repo, len(config.PerRepoIdentities))
		}
	}

	if trigger == periapsis.TriggerCI {
		if identity, ok := config.identityForPerRepoCI(upstreamName, org, repo); ok {
			return identity, nil
		}
		// The shared ci-secrets SA's Vault policy (OberthCISecretsPolicy) is
		// an org-union: path "oberth/data/upstream/<org>/*" for EVERY
		// registered org. When other repos have CI per-repo identities, falling
		// back to the shared SA would give this repo's CI run read access to
		// every other org's upstream secrets. Refuse admission and direct the
		// operator to provision per-repo CI identities. The single-repo case
		// (no CI per-repo identities) is safe: the shared policy carries all
		// orgs but there is only one repo with CI secrets, so the exposure is
		// limited to its own org's subtree. (Issue #433)
		if len(config.PerRepoCIIdentities) > 0 {
			return argoworkflow.Identity{}, fmt.Errorf(
				"argojob: refusing CI submission for %q: %d other repo(s) hold per-repo "+
					"CI identities with scoped upstream access, and this repo has no per-repo CI identity; "+
					"the shared ci-secrets service account's Vault policy includes all orgs' upstream paths, "+
					"so falling back to it would leak cross-org secrets; "+
					"run \"oberth secretstore sync\" (or \"oberth install --install-secretstore --upgrade\") to provision per-repo CI identities",
				repo, len(config.PerRepoCIIdentities))
		}
	}

	// Fall back to shared tier identity.
	return config.identityFor(trigger, true)
}

// vaultRoleForWithRepo selects the Vault role, consulting per-repo roles for
// both the RELEASE and CI triggers. The two selections (identity and role)
// must stay in lockstep: a shared-SA pod told to log in with a per-repo role
// would merely fail at Vault (bound_service_account_names mismatch), but the
// admission record would misstate the run's reachable credentials.
func (config Config) vaultRoleForWithRepo(trigger periapsis.Trigger, upstreamName, org, repo string) string {
	if trigger == periapsis.TriggerRelease {
		if role := config.vaultRoleForPerRepo(upstreamName, org, repo); role != "" {
			return role
		}
	}
	if trigger == periapsis.TriggerCI {
		if role := config.vaultRoleForPerRepoCI(upstreamName, org, repo); role != "" {
			return role
		}
	}
	return config.vaultRoleFor(trigger)
}
