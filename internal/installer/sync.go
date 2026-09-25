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
