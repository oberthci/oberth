package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

const (
	secretAccessConfigMapName = "oberth-secret-access" // #nosec G101 — Kubernetes ConfigMap NAME (an identifier), not credential material.
	secretAccessConfigMapKey  = "grants"
)

// grantEntry mirrors service.SecretAccessGrantEntry for ConfigMap parsing
// without importing the service package (which would pull in transitive
// dependencies the installer does not need).
type grantEntry struct {
	Repo   string `json:"repo"   yaml:"repo"`
	Step   string `json:"step"   yaml:"step"`
	Secret string `json:"secret" yaml:"secret"`
}

// accessListJSONEntry is the local deserialization target for the --json
// output of `oberth access list`. It mirrors the wire schema defined in
// cmd/oberth/admin.go's accessListJSONGrant without importing the main
// package. Only Repo and Secret are consumed for identity production.
type accessListJSONEntry struct {
	Repo   string `json:"repo"`
	Step   string `json:"step"`
	Secret string `json:"secret"`
}

// ProducePerRepoIdentities reads per-repo identity data needed for Vault
// policy provisioning. It first tries to query the running Oberth server's
// database by exec'ing `oberth access list --json` in the Oberth deployment
// pod. If that flag is rejected (older deployed binary), it retries with the
// tabwriter format. If both exec attempts fail (fresh install, pod not yet
// running), it falls back to reading the secret-access ConfigMap directly —
// that path only accepts qualified entries because the ConfigMap may carry
// bare-spelled names the installer cannot resolve without database access.
//
// The returned warnings slice carries degradation notices (e.g. exec failed,
// fell back to ConfigMap) that the caller should surface in its output.
func ProducePerRepoIdentities(ctx context.Context, kube kubernetes.Interface, run CommandRunner, contextName, namespace string) ([]PerRepoIdentity, []string, error) {
	var warnings []string

	if run != nil {
		identities, execWarnings, err := produceFromAccessList(ctx, run, contextName, namespace)
		warnings = append(warnings, execWarnings...)
		if err == nil {
			return identities, warnings, nil
		}
		// exec failed — fall through to the ConfigMap path. Record the
		// degradation so the caller can surface it.
		cmIdentities, cmErr := produceFromConfigMap(ctx, kube, namespace)
		if cmErr != nil {
			return nil, warnings, cmErr
		}
		warnings = append(warnings, fmt.Sprintf("access list exec failed (%v); fell back to ConfigMap (%d qualified entries)", err, len(cmIdentities)))
		return cmIdentities, warnings, nil
	}

	identities, err := produceFromConfigMap(ctx, kube, namespace)
	return identities, warnings, err
}

// produceFromAccessList execs into the running Oberth deployment pod and
// runs `oberth access list` to read per-repo grant data from the server's
// database. It first attempts --json for structured parsing; if the deployed
// binary rejects the flag (older version), it retries with the tabwriter
// format. The database rows carry qualified "upstream/org/repo" names
// thanks to the v12 schema migration and the access reconciler.
func produceFromAccessList(ctx context.Context, run CommandRunner, contextName, namespace string) ([]PerRepoIdentity, []string, error) {
	baseArgs := []string{"exec", "-i", "-c", "oberth"}
	if contextName != "" {
		baseArgs = append(baseArgs, "--context", contextName)
	}
	baseArgs = append(baseArgs, "-n", namespace, "deploy/oberth", "--", "oberth", "access", "list")

	// Try --json first (available since the version shipping this code).
	jsonArgs := append(append([]string(nil), baseArgs...), "--json")
	out, err := run(ctx, nil, "kubectl", jsonArgs...)
	if err == nil {
		identities, parseErr := ParseAccessListJSON(out)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parse access list --json output: %w", parseErr)
		}
		return identities, nil, nil
	}

	// --json rejected (older deployed binary) — retry with tabwriter format.
	var warnings []string
	warnings = append(warnings, fmt.Sprintf("access list --json exec failed (%v); retrying with tabwriter format", err))

	out, err = run(ctx, nil, "kubectl", baseArgs...)
	if err != nil {
		return nil, warnings, fmt.Errorf("exec access list in Oberth pod: %w", err)
	}

	identities, parseErr := ParseAccessListOutput(out)
	if parseErr != nil {
		return nil, warnings, parseErr
	}
	return identities, warnings, nil
}

// parseAccessListJSON parses the JSON-formatted output of `oberth access list
// --json` into per-repo identities. Only qualified 3-segment repo names
// produce identities; bare rows are skipped. Returns an error if the array
// has entries but zero survive the 3-segment filter (format drift detection).
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
		parts := strings.Split(entry.Repo, "/")
		if len(parts) != 3 {
			continue // skip bare names
		}
		key := repoKey{upstream: parts[0], org: parts[1], repo: parts[2]}
		byRepo[key] = append(byRepo[key], entry.Secret)
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

// accessListSeparator matches the 3+ space padding tabwriter inserts between
// columns. Column values (repo, step, secret) never contain runs of 3+
// spaces, so this is a reliable field boundary.
var accessListSeparator = regexp.MustCompile(`\s{3,}`)

// parseAccessListOutput parses the tabwriter-formatted output of
// `oberth access list` into per-repo identities. Each line has:
//
//	REPO   STEP   SECRET   APPROVED_BY   APPROVED_AT   STATUS
//
// separated by 3+ spaces (tabwriter padding). Only qualified 3-segment
// repo names produce identities; unresolved bare rows in the database
// (possible for repos unregistered at both the v12 migration and the last
// reconcile) are skipped.
//
// Returns an error if there are data lines but zero identities survive the
// 3-segment filter (format drift detection).
func ParseAccessListOutput(data []byte) ([]PerRepoIdentity, error) {
	type repoKey struct {
		upstream, org, repo string
	}
	byRepo := make(map[repoKey][]string)

	var dataLines int
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "REPO") {
			continue // skip header and blank lines
		}
		dataLines++

		fields := accessListSeparator.Split(line, -1)
		if len(fields) < 3 {
			continue
		}

		repo := strings.TrimSpace(fields[0])
		secret := strings.TrimSpace(fields[2])

		parts := strings.Split(repo, "/")
		if len(parts) != 3 {
			continue // skip bare names that the DB hasn't qualified yet
		}

		key := repoKey{upstream: parts[0], org: parts[1], repo: parts[2]}
		byRepo[key] = append(byRepo[key], secret)
	}

	if dataLines > 0 && len(byRepo) == 0 {
		return nil, fmt.Errorf("access list output yielded no identities from %d data lines — format drift?", dataLines)
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

// produceFromConfigMap reads the secret-access ConfigMap and builds per-repo
// identities from entries whose repo field is already in qualified
// "upstream/org/repo" format. Bare-name entries are skipped because the
// ConfigMap may not have been canonicalized yet and the installer has no
// database access to resolve them.
//
// Only a genuine not-found error (fresh install, no ConfigMap yet) returns
// nil, nil. Other errors (RBAC, transport) are surfaced so the caller's
// warning path is reachable.
func produceFromConfigMap(ctx context.Context, kube kubernetes.Interface, namespace string) ([]PerRepoIdentity, error) {
	cm, err := kube.CoreV1().ConfigMaps(namespace).Get(ctx, secretAccessConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// ConfigMap not found: no per-repo identities. Normal for
			// fresh installs where no grants exist yet.
			return nil, nil
		}
		// RBAC, transport, or other non-retriable error — surface it
		// instead of silently returning zero identities.
		return nil, fmt.Errorf("read secret access ConfigMap: %w", err)
	}

	data := cm.Data[secretAccessConfigMapKey]
	if strings.TrimSpace(data) == "" {
		return nil, nil
	}

	var entries []grantEntry
	if err := yaml.Unmarshal([]byte(data), &entries); err != nil {
		return nil, fmt.Errorf("parse secret access ConfigMap: %w", err)
	}

	// Group grant secrets by qualified repo key.
	type repoKey struct {
		upstream, org, repo string
	}
	type repoGrants struct {
		key    repoKey
		grants []string
	}
	byRepo := make(map[string]*repoGrants)

	for _, entry := range entries {
		parts := strings.Split(entry.Repo, "/")
		if len(parts) != 3 {
			// Skip bare or org/repo entries — they can't be unambiguously
			// resolved without database access.
			continue
		}
		upstream, org, repo := parts[0], parts[1], parts[2]
		mapKey := entry.Repo

		rg, exists := byRepo[mapKey]
		if !exists {
			rg = &repoGrants{key: repoKey{upstream: upstream, org: org, repo: repo}}
			byRepo[mapKey] = rg
		}
		rg.grants = append(rg.grants, entry.Secret)
	}

	var result []PerRepoIdentity
	for _, rg := range byRepo {
		result = append(result, PerRepoIdentity{
			Upstream: rg.key.upstream,
			Org:      rg.key.org,
			Repo:     rg.key.repo,
			Grants:   rg.grants,
		})
	}
	return result, nil
}
