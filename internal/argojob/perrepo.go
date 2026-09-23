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

// identityForWithRepo selects the ServiceAccount for a run, consulting
// per-repo identities for the RELEASE trigger only.
//
// The tier restriction is the trust boundary, not an implementation detail:
// a per-repo identity's Vault policy carries the repo's approval-table
// grants — release credentials (issue #200). The shared model keeps branch
// (CI) runs on the ci-secrets identity, whose policy is structurally
// grant-free, so repository-authored code in a branch push can never reach
// release secrets no matter what it declares. Selecting the per-repo
// identity for a CI run would put those grants inside the pod's reachable
// policy and collapse exactly that boundary. Until a separate grant-free
// CI-tier per-repo identity family exists (tier-split, deferred from #246),
// CI triggers always use the shared ci-secrets identity.
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

	// Per-repo identities apply to the release tier only (see above).
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
					"run \"oberth install --install-secretstore --upgrade\" to provision per-repo identities",
				repo, len(config.PerRepoIdentities))
		}
	}

	// Fall back to shared tier identity.
	return config.identityFor(trigger, true)
}

// vaultRoleForWithRepo selects the Vault role, consulting per-repo roles for
// the RELEASE trigger only — the same tier restriction, for the same #200
// reason, as identityForWithRepo. The two selections must stay in lockstep:
// a shared-SA pod told to log in with a per-repo role would merely fail at
// Vault (bound_service_account_names mismatch), but the admission record
// would misstate the run's reachable credentials.
func (config Config) vaultRoleForWithRepo(trigger periapsis.Trigger, upstreamName, org, repo string) string {
	if trigger == periapsis.TriggerRelease {
		if role := config.vaultRoleForPerRepo(upstreamName, org, repo); role != "" {
			return role
		}
	}
	return config.vaultRoleFor(trigger)
}
