package installer

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/identityname"
)

// RunSync re-derives every Vault policy and role that depends on the approval
// table — per-repo identities (release + CI tiers), per-step identities
// (issue #623), and the shared credentialed and CI-secrets policies — without
// touching anything else (no mounts, no transit keys, no chart install, no
// helm). It runs under the administrator's own BAO_TOKEN, exactly as the full
// `install --install-secretstore --upgrade` path does.
//
// This is the entry point that `oberth secretstore sync` dispatches to. It
// creates the internal openBaoExec from the provided CommandRunner and
// cluster coordinates, then calls the internal syncGrantPolicies.
func RunSync(ctx context.Context, run CommandRunner, rootToken, contextName, openbaoNamespace, openbaoPod, argoNamespace string, identities []PerRepoIdentity, stepIdentities []PerStepIdentity, output io.Writer) ([]SyncResult, error) {
	store := openBaoExec{
		run:         run,
		contextName: contextName,
		namespace:   openbaoNamespace,
		pod:         openbaoPod,
	}
	return syncGrantPolicies(ctx, store, rootToken, identities, stepIdentities, argoNamespace, output)
}

// syncGrantPolicies re-derives every Vault policy and role that depends on
// the approval table. It is idempotent: policies that already match the
// expected shape are left untouched and reported as unchanged.
func syncGrantPolicies(ctx context.Context, store openBaoExec, rootToken string, identities []PerRepoIdentity, stepIdentities []PerStepIdentity, argoNamespace string, output io.Writer) ([]SyncResult, error) {
	var results []SyncResult

	// Validate upstream org names derived from identities. The shared policies
	// no longer carry these (stripped to zero stanzas below), but early
	// validation catches bad names before ConfigurePerRepoIdentities — defense
	// in depth for the per-repo path.
	upstreamOrgs := upstreamOrgsFromIdentities(identities)
	for _, org := range upstreamOrgs {
		if err := ValidateOrgName(org); err != nil {
			return results, fmt.Errorf("upstream org validation: %w", err)
		}
	}

	// --- Shared credentialed policy ---
	//
	// Always zero stanzas: no upstream org access, no approval-table grants.
	// Admission never selects the shared credentialed identity once per-repo
	// identities exist (fail-closed in identityForWithRepo), so any residual
	// breadth is dormant standing privilege — strip it rather than re-widen
	// on every sync. When no identities exist, the policies are naturally
	// empty (fail closed from zero orgs). Issue #456. This also resolves the
	// sync/install ping-pong where sync wrote the grant-union and install
	// wrote grant-free — both now produce the same zero-stanza shape.
	wantCredentialedPolicy := OberthCredentialedPolicyWithGrants(defaultKVPrefix, nil, nil)
	haveCredentialedPolicy, credentialedExists, err := store.policyRead(ctx, rootToken, defaultCredentialedPolicy)
	if err != nil {
		return results, fmt.Errorf("read credentialed policy: %w", err)
	}
	credentialedChanged := !credentialedExists || strings.TrimSpace(haveCredentialedPolicy) != strings.TrimSpace(wantCredentialedPolicy)
	if credentialedChanged {
		if err := store.policyWrite(ctx, rootToken, defaultCredentialedPolicy, wantCredentialedPolicy); err != nil {
			return results, fmt.Errorf("write credentialed policy: %w", err)
		}
	}
	results = append(results, SyncResult{
		Name:    "credentialed policy",
		Changed: credentialedChanged,
	})

	// --- Shared CI-secrets policy (same zero-stanza treatment as above) ---
	wantCISecretsPolicy := OberthCISecretsPolicy(defaultKVPrefix, nil)
	haveCISecretsPolicy, ciSecretsExists, err := store.policyRead(ctx, rootToken, defaultCISecretsPolicy)
	if err != nil {
		return results, fmt.Errorf("read ci-secrets policy: %w", err)
	}
	ciSecretsChanged := !ciSecretsExists || strings.TrimSpace(haveCISecretsPolicy) != strings.TrimSpace(wantCISecretsPolicy)
	if ciSecretsChanged {
		if err := store.policyWrite(ctx, rootToken, defaultCISecretsPolicy, wantCISecretsPolicy); err != nil {
			return results, fmt.Errorf("write ci-secrets policy: %w", err)
		}
	}
	results = append(results, SyncResult{
		Name:    "ci-secrets policy",
		Changed: ciSecretsChanged,
	})

	// --- Per-repo identities (release + CI tiers) ---
	if len(identities) > 0 {
		perRepoItems, err := ConfigurePerRepoIdentities(ctx, store, rootToken, identities, argoNamespace)
		if err != nil {
			return results, fmt.Errorf("per-repo identities: %w", err)
		}
		for _, item := range perRepoItems {
			results = append(results, SyncResult{
				Name:    item.Name,
				Changed: item.Changed,
			})
		}
	}

	// --- Per-step identities (issue #623) ---
	// For each (repo, step) with named grants, render a dedicated SA+policy+role
	// scoped to exactly that step's paths. Wildcard grants keep the per-repo
	// identity (backward compatible).
	desiredStepNames := make(map[string]struct{})
	if len(stepIdentities) > 0 {
		perStepItems, err := ConfigurePerStepIdentities(ctx, store, rootToken, stepIdentities, argoNamespace)
		if err != nil {
			return results, fmt.Errorf("per-step identities: %w", err)
		}
		for _, item := range perStepItems {
			results = append(results, SyncResult{
				Name:    item.Name,
				Changed: item.Changed,
			})
		}
		for _, sid := range stepIdentities {
			desiredStepNames[PerStepName(sid.Upstream, sid.Org, sid.Repo, sid.Step)] = struct{}{}
		}
	}

	// Remove both tiers of per-repo identities and named-step identities that
	// disappeared from the approval table. Listing is part of the sync's
	// success condition: a receipt cannot claim revoked grants were removed
	// when the store could not enumerate their policies.
	desiredNames := desiredStepNames
	// The shared policies use the same oberth-argo- prefix but are always
	// managed above, independently of the per-repository approval table.
	desiredNames[defaultCredentialedPolicy] = struct{}{}
	desiredNames[defaultCISecretsPolicy] = struct{}{}
	for _, id := range identities {
		desiredNames[PerRepoName(id.Upstream, id.Org, id.Repo)] = struct{}{}
		desiredNames[PerRepoCIName(id.Upstream, id.Org, id.Repo)] = struct{}{}
	}
	existingPolicies, err := store.policyList(ctx, rootToken)
	if err != nil {
		return results, fmt.Errorf("list policies for orphan removal: %w", err)
	}
	existingRoles, err := store.roleList(ctx, rootToken)
	if err != nil {
		return results, fmt.Errorf("list roles for orphan removal: %w", err)
	}
	// A previous sync may have deleted a policy but failed to remove its role.
	// Enumerate both sides, so those roles remain discoverable and retryable.
	existing := make(map[string]struct{})
	for _, name := range append(existingPolicies, existingRoles...) {
		existing[name] = struct{}{}
	}
	names := make([]string, 0, len(existing))
	for name := range existing {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		kind := "per-repo"
		if identityname.HasPerStepPrefix(name) {
			kind = "per-step"
		} else if !strings.HasPrefix(name, identityname.PerRepoPrefix) {
			continue
		}
		if _, desired := desiredNames[name]; desired {
			continue
		}
		_, _ = fmt.Fprintf(output, "  removing orphan %s identity %s (not in desired set)\n", kind, name)
		// Keep the policy discoverable until role deletion succeeds, so a
		// partial failure can be retried by the next sync.
		rolePath := "auth/" + defaultAuthMount + "/role/" + name
		if err := store.deleteRole(ctx, rootToken, rolePath); err != nil {
			return results, fmt.Errorf("remove orphan %s role %s: %w", kind, name, err)
		}
		if err := store.policyDelete(ctx, rootToken, name); err != nil {
			return results, fmt.Errorf("remove orphan %s policy %s: %w", kind, name, err)
		}
		results = append(results, SyncResult{Name: "removed " + kind + " " + name, Changed: true})
	}

	return results, nil
}

// SyncResult describes the outcome of syncing one policy or role.
type SyncResult struct {
	// Name identifies the object (e.g. "credentialed policy",
	// "per-repo policy oberth-argo-codeberg-oberthci-oberth-abc123").
	Name string
	// Changed reports whether the object was written. When false, the
	// existing object already matched the expected shape.
	Changed bool
}

// SyncReceipt is the typed receipt that runSecretStoreSync posts back to the
// running Oberth server after a successful sync. It records exactly what was
// materialized without carrying any secret values (#712).
type SyncReceipt struct {
	// PlanDigest is the sha256 plan digest computed from the same approval table
	// the sync was derived from. It is the same digest secretstore_plan prints.
	PlanDigest string `json:"plan_digest"`
	// Policies maps each policy/role name to whether it was changed.
	Policies map[string]bool `json:"policies"`
	// ChangedCount is the number of policies/roles that were written.
	ChangedCount int `json:"changed_count"`
	// TotalCount is the total number of policies/roles evaluated.
	TotalCount int `json:"total_count"`
}

// ComputeSyncReceipt builds a typed receipt from sync results and a plan digest.
// The receipt never contains secret values — only names and changed/unchanged
// status. The plan digest is the same value secretstore_plan returns, computed
// from the approval table at the time of the sync.
func ComputeSyncReceipt(results []SyncResult, planDigest string) SyncReceipt {
	policies := make(map[string]bool, len(results))
	changed := 0
	for _, r := range results {
		policies[r.Name] = r.Changed
		if r.Changed {
			changed++
		}
	}
	return SyncReceipt{
		PlanDigest:   planDigest,
		Policies:     policies,
		ChangedCount: changed,
		TotalCount:   len(results),
	}
}
