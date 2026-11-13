package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/model"
	"sort"
	"strings"
)

const (
	secretAccessConfigMapName = "oberth-secret-access" // #nosec G101 — Kubernetes ConfigMap NAME (an identifier), not credential material.
	secretAccessConfigMapKey  = "grants"

	// wildcardStep is the approval-table step value that grants a path to
	// every template of a repository. Any other value names one Argo template.
	wildcardStep = "*"
)

// appendPerRepoGrant adds secret to a per-repo identity's grant list only when
// the grant is a wildcard grant. A named-step grant belongs to the per-step
// identity alone (DerivePerStepIdentities): the per-repo ServiceAccount is the
// workflow default for every template without a named grant, so a path it can
// read is a path every such template can read from Vault with its own token.
// Carrying named-step paths here made the per-step boundary of issue #623
// exist only at admission, not at the Vault layer, and meant revoking a
// wildcard grant could never narrow the per-repo policy while a named grant
// for the same path remained (issue #814).
//
// The caller assigns the result back into its map even when nothing was
// appended, so a repository whose grants are all named still produces a
// per-repo identity (with an empty grant list): its ServiceAccount is still
// the workflow default and the chart's SA list depends on it.
func appendPerRepoGrant(grants []string, step, secret string) []string {
	if step != wildcardStep {
		return grants
	}
	return append(grants, secret)
}

// grantEntry mirrors service.SecretAccessGrantEntry for ConfigMap parsing
// without importing the service package (which would pull in transitive
// dependencies the installer does not need).
type grantEntry struct {
	Workflow string `json:"workflow" yaml:"workflow"`
	Repo     string `json:"repo"   yaml:"repo"`
	Step     string `json:"step"   yaml:"step"`
	Secret   string `json:"secret" yaml:"secret"`
}

// accessListJSONEntry is the local deserialization target for the --json
// output of `oberth access list`. It mirrors the wire schema defined in
// cmd/oberth/admin.go's accessListJSONGrant without importing the main
// package. Only Repo and Secret are consumed for identity production.
type accessListJSONEntry struct {
	Workflow string `json:"workflow" yaml:"workflow"`
	Repo     string `json:"repo"`
	Step     string `json:"step"`
	Secret   string `json:"secret"`
}

// ProducePerRepoIdentities derives identities from the authenticated active grant catalog.
func ProducePerRepoIdentities(ctx context.Context, cfg Config, deps Deps) ([]PerRepoIdentity, []string, error) {
	data, err := activeGrantJSON(ctx, cfg, deps)
	if err != nil {
		return nil, nil, err
	}
	identities, err := ParseAccessListJSON(data)
	return identities, nil, err
}

// parseAccessListJSON parses the JSON-formatted output of `oberth access list
// --json` into per-repo identities. Only qualified 3-segment repo names
// produce identities; bare rows are skipped. Returns an error if the array
// has entries but zero survive the 3-segment filter (format drift detection).
// Per-repo grants carry wildcard-step rows only; named-step rows reach the
// per-step identities through ParseAccessListJSONWithSteps (issue #814).
func ParseAccessListJSON(data []byte) ([]PerRepoIdentity, error) {
	var entries []accessListJSONEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("unmarshal access list JSON: %w", err)
	}

	type repoKey struct {
		upstream, org, repo string
	}
	byRepo := make(map[repoKey][]string)

	for _, entry := range entries {
		if err := model.ValidateWorkflowFile(entry.Workflow); err != nil {
			return nil, fmt.Errorf("unscoped grant: %w", err)
		}
		parts := strings.Split(entry.Repo, "/")
		if len(parts) != 3 {
			continue // skip bare names
		}
		key := repoKey{upstream: parts[0], org: parts[1], repo: parts[2]}
		byRepo[key] = nil
	}

	if len(entries) > 0 && len(byRepo) == 0 {
		return nil, fmt.Errorf("access list JSON output yielded no identities from %d entries — format drift?", len(entries))
	}

	result := make([]PerRepoIdentity, 0, len(byRepo))
	for key, grants := range byRepo {
		sort.Strings(grants)
		result = append(result, PerRepoIdentity{
			Upstream: key.upstream,
			Org:      key.org,
			Repo:     key.repo,
			Grants:   grants,
		})
	}
	return result, nil
}

// ParseAccessListJSONWithSteps parses the JSON output and returns both per-repo
// identities and the raw grant entries with step information needed to derive
// per-step identities. This is the structured entry point for the sync command
// to produce both per-repo and per-step identities in one pass. The two path
// sets are disjoint by construction: a per-repo identity carries only the
// wildcard-step grants, a per-step identity carries its named grants plus the
// inherited wildcards (issue #814).
func ParseAccessListJSONWithSteps(data []byte) ([]PerRepoIdentity, []grantWithStep, error) {
	var entries []accessListJSONEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, nil, fmt.Errorf("unmarshal access list JSON: %w", err)
	}

	type repoKey struct {
		upstream, org, repo string
	}
	byRepo := make(map[repoKey][]string)
	var grants []grantWithStep

	for _, entry := range entries {
		if err := model.ValidateWorkflowFile(entry.Workflow); err != nil {
			return nil, nil, fmt.Errorf("unscoped grant: %w", err)
		}
		parts := strings.Split(entry.Repo, "/")
		if len(parts) != 3 {
			continue
		}
		key := repoKey{upstream: parts[0], org: parts[1], repo: parts[2]}
		byRepo[key] = nil
		grants = append(grants, grantWithStep{
			upstream: parts[0],
			org:      parts[1],
			repo:     parts[2],
			step:     entry.Step,
			workflow: entry.Workflow,
			secret:   entry.Secret,
		})
	}

	if len(entries) > 0 && len(byRepo) == 0 {
		return nil, nil, fmt.Errorf("access list JSON output yielded no identities from %d entries — format drift?", len(entries))
	}

	repoIdentities := make([]PerRepoIdentity, 0, len(byRepo))
	for key, repoGrants := range byRepo {
		sort.Strings(repoGrants)
		repoIdentities = append(repoIdentities, PerRepoIdentity{
			Upstream: key.upstream,
			Org:      key.org,
			Repo:     key.repo,
			Grants:   repoGrants,
		})
	}
	return repoIdentities, grants, nil
}

// ProducePerStepIdentities reads the access list from the running server and
// derives per-step identities. For each named-step grant (step != "*"), a
// per-step identity is produced with paths = the step's named grants PLUS any
// wildcard-granted paths for the same repo (inherited). Issue #623, finding 1.
func ProducePerStepIdentities(ctx context.Context, cfg Config, deps Deps) ([]PerStepIdentity, error) {
	data, err := activeGrantJSON(ctx, cfg, deps)
	if err != nil {
		return nil, err
	}
	_, grants, err := ParseAccessListJSONWithSteps(data)
	if err != nil {
		return nil, err
	}
	return DerivePerStepIdentities(nil, grants), nil
}

// produceFromConfigMap reads the secret-access ConfigMap and builds per-repo
// identities from entries whose repo field is already in qualified
// "upstream/org/repo" format. Bare-name entries are skipped because the
// ConfigMap may not have been canonicalized yet and the installer has no
// database access to resolve them.
//
// Only a genuine not-found error (fresh install, no ConfigMap yet) returns
// nil, nil. Other errors (RBAC, transport) are surfaced so the caller's
// warning path is reachable.

// ProducePerRepoIdentitiesForSync reads the desired set for destructive reconciliation.
func ProducePerRepoIdentitiesForSync(ctx context.Context, cfg Config, deps Deps) ([]PerRepoIdentity, []string, error) {
	return ProducePerRepoIdentities(ctx, cfg, deps)
}

func activeGrantJSON(ctx context.Context, cfg Config, deps Deps) ([]byte, error) {
	var result struct {
		Grants []accessListJSONEntry `json:"grants"`
	}
	if err := CallAdmin(ctx, cfg, deps, "access-list", nil, &result); err != nil {
		return nil, err
	}
	if result.Grants == nil {
		return nil, fmt.Errorf("admin access list omitted the grants array")
	}
	return json.Marshal(result.Grants) //nolint:gosec // G117: Grant entries contain secret path identifiers, never secret values.
}
