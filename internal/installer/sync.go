package installer

import (
	"context"
	"fmt"
	"strings"
)

// RunSync re-derives every Vault policy and role that depends on the approval
// table — per-repo identities (release + CI tiers) and the shared
// credentialed and CI-secrets policies — without touching anything else (no
// mounts, no transit keys, no chart install, no helm). It runs under the
// administrator's own BAO_TOKEN, exactly as the full
// `install --install-secretstore --upgrade` path does.
//
// This is the entry point that `oberth secretstore sync` dispatches to. It
// creates the internal openBaoExec from the provided CommandRunner and
// cluster coordinates, then calls the internal syncGrantPolicies.
func RunSync(ctx context.Context, run CommandRunner, rootToken, contextName, openbaoNamespace, openbaoPod, argoNamespace string, identities []PerRepoIdentity) ([]SyncResult, error) {
	store := openBaoExec{
		run:         run,
		contextName: contextName,
		namespace:   openbaoNamespace,
		pod:         openbaoPod,
	}
	return syncGrantPolicies(ctx, store, rootToken, identities, argoNamespace)
}

// syncGrantPolicies re-derives every Vault policy and role that depends on
// the approval table. It is idempotent: policies that already match the
// expected shape are left untouched and reported as unchanged.
func syncGrantPolicies(ctx context.Context, store openBaoExec, rootToken string, identities []PerRepoIdentity, argoNamespace string) ([]SyncResult, error) {
	var results []SyncResult

	// Derive upstream orgs from identities for the shared policies.
	upstreamOrgs := upstreamOrgsFromIdentities(identities)
	for _, org := range upstreamOrgs {
		if err := ValidateOrgName(org); err != nil {
			return results, fmt.Errorf("upstream org validation: %w", err)
		}
	}

	// --- Shared credentialed policy ---
	//
	// Aggregate all grant paths from all per-repo identities. These feed the
	// shared credentialed policy exactly as --credentialed-secret-path does
	// in the install flow, so the shared SA has access to the union of all
	// approved paths.
	var allGrantPaths []string
	for _, id := range identities {
		allGrantPaths = append(allGrantPaths, id.Grants...)
	}
	credentialedGrantPaths, err := credentialedPolicyPaths(defaultKVPrefix, allGrantPaths)
	if err != nil {
		return results, fmt.Errorf("credentialed policy paths: %w", err)
	}
	wantCredentialedPolicy := OberthCredentialedPolicyWithGrants(defaultKVPrefix, upstreamOrgs, credentialedGrantPaths)
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

	// --- Shared CI-secrets policy ---
	wantCISecretsPolicy := OberthCISecretsPolicy(defaultKVPrefix, upstreamOrgs)
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
