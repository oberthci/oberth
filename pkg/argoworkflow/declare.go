package argoworkflow

import (
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// Repository declarations the server reads statically from the decoded
// document. They are the Argo-path counterparts of pkg/periapsis's
// DeclaredJobSizes and DeclaredSecretStoreSecrets: annotations rather than Go
// literals, but the same rule applies — the server reads them, never evaluates
// them, and the reviewed source and the admission decision cannot diverge.
const (
	// SizeAnnotation declares the scheduler admission weight of this pipeline,
	// one of S/M/L/XL. Absent selects M.
	//
	// Unlike the Go path, this does not size any container: an Argo template
	// carries its own resources block. It only tells Oberth's weighted queue
	// how much of the node this run intends to occupy.
	//
	// The declaration costs concurrency, so declare what the pipeline needs
	// rather than the largest tier that looks safe. Against a server
	// configured for N concurrent jobs, S and M both run N at a time, L runs
	// floor(2N/3), and XL runs alone. Declaring L on a server set to 3 buys
	// per-run headroom at the price of one of the three slots; declaring XL
	// costs two of them.
	SizeAnnotation = "oberth.ci/size"

	// ConcurrencyGroupAnnotation names a concurrency group this pipeline's
	// runs belong to: at most one run of the same repository and group
	// executes at a time, across branch, promotion, and tag triggers, and the
	// others queue behind it in claim order. Absent declares no group.
	//
	// The group is a mutual exclusion, not a size. It never changes the run's
	// admission weight, so a repository that must serialize its own work (a
	// Terraform state, say) declares its real size and the group rather than
	// XL: unrelated repositories keep running beside it. Groups are scoped to
	// the declaring repository, so a document in another repository naming the
	// same group cannot block this one. Names are 1 to 63 characters of
	// lowercase letters, digits, and hyphens; anything else fails admission.
	ConcurrencyGroupAnnotation = "oberth.ci/concurrency-group"

	// SecretPathsAnnotation declares, comma-separated, the OpenBao KV paths
	// this pipeline's credentialed steps intend to read in-Pod through
	// envconsul. Both CI and release pipelines may declare paths; the
	// authorization boundary is the allowlist and the OpenBao Kubernetes-auth
	// role binding, not the trigger type.
	//
	// AI-CONTRACT: this declaration is auditable intent, not enforcement. The
	// authoritative boundary is the Vault Kubernetes-auth role bound to the
	// release ServiceAccount and the policy attached to it. Oberth records the
	// declaration in its audit chain at submission so the intended and the
	// granted set can be compared; it cannot observe the actual read, because
	// on this tier the value never passes through the server.
	SecretPathsAnnotation = "oberth.ci/secret-paths"
)

const maxDeclaredSecretPaths = 32

// MaxConcurrencyGroupLength bounds a concurrency group name.
const MaxConcurrencyGroupLength = 63

// ValidConcurrencyGroup reports whether name is a legal concurrency group:
// 1 to 63 characters, each a lowercase ASCII letter, digit, or hyphen. The
// store's schema enforces the same rule, so a name that passes here is the
// only kind that can ever be recorded on a run.
func ValidConcurrencyGroup(name string) bool {
	if name == "" || len(name) > MaxConcurrencyGroupLength {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

// DeclaredConcurrencyGroup returns the concurrency group the document declares,
// or "" when it declares none. Surrounding whitespace is ignored exactly as it
// is for the size annotation; the name itself is never normalized, so a value
// that is not already a legal group name is an admission error rather than a
// silently different group.
func DeclaredConcurrencyGroup(workflow *wfv1.Workflow) (string, error) {
	if workflow == nil {
		return "", errors.New("argoworkflow: no Workflow to read")
	}
	declared, ok := workflow.Annotations[ConcurrencyGroupAnnotation]
	if !ok || strings.TrimSpace(declared) == "" {
		return "", nil
	}
	group := strings.TrimSpace(declared)
	if !ValidConcurrencyGroup(group) {
		return "", fmt.Errorf("argoworkflow: annotation %s declares invalid concurrency group %q; "+
			"a group is 1 to %d characters of lowercase letters, digits, and hyphens ([a-z0-9-])",
			ConcurrencyGroupAnnotation, declared, MaxConcurrencyGroupLength)
	}
	return group, nil
}

// DeclaredSize returns the scheduler admission weight the document requests.
func DeclaredSize(workflow *wfv1.Workflow) (periapsis.Size, error) {
	if workflow == nil {
		return "", errors.New("argoworkflow: no Workflow to read")
	}
	declared, ok := workflow.Annotations[SizeAnnotation]
	if !ok || strings.TrimSpace(declared) == "" {
		return periapsis.M, nil
	}
	size := periapsis.Size(strings.TrimSpace(declared))
	switch size {
	case periapsis.S, periapsis.M, periapsis.L, periapsis.XL:
		return size, nil
	default:
		return "", fmt.Errorf("argoworkflow: annotation %s declares unknown size %q; sizes are %q, %q, %q, and %q",
			SizeAnnotation, declared, periapsis.S, periapsis.M, periapsis.L, periapsis.XL)
	}
}

// DeclaredSecretPaths returns the OpenBao KV paths the document declares it
// will read in-Pod, in declaration order with duplicates rejected.
func DeclaredSecretPaths(workflow *wfv1.Workflow) ([]string, error) {
	if workflow == nil {
		return nil, errors.New("argoworkflow: no Workflow to read")
	}
	declared, ok := workflow.Annotations[SecretPathsAnnotation]
	if !ok || strings.TrimSpace(declared) == "" {
		return nil, nil
	}
	fields := strings.Split(declared, ",")
	if len(fields) > maxDeclaredSecretPaths {
		return nil, fmt.Errorf("argoworkflow: annotation %s declares %d paths, maximum is %d",
			SecretPathsAnnotation, len(fields), maxDeclaredSecretPaths)
	}
	paths := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for index, field := range fields {
		path := strings.TrimSpace(field)
		if err := periapsis.ValidateSecretStorePath(path); err != nil {
			return nil, fmt.Errorf("argoworkflow: annotation %s entry %d: %w", SecretPathsAnnotation, index, err)
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, fmt.Errorf("argoworkflow: annotation %s declares %q more than once", SecretPathsAnnotation, path)
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths, nil
}

// AuthorizeSecretPaths proves every declared path is one this repository may
// reach for this trigger, reusing the Go path's hierarchical scoping rules so
// both authoring formats answer to the same namespace policy.
//
// systemAllowlist is the administrator's exact-path allowlist for the
// oberth/data/ system namespace; oberth/upstream/ paths are authorized by the
// declaring repository's own org and name instead and are never allowlisted.
//
// CI pipelines may only declare upstream-scoped paths (oberth/upstream/...).
// System-namespace paths (oberth/data/release/...) are CI-ineligible: a branch
// push must never be able to reach release credentials, regardless of what the
// document asks for. The release trigger remains unrestricted.
func AuthorizeSecretPaths(paths []string, systemAllowlist []string, trigger periapsis.Trigger, org, repo string) error {
	if len(paths) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(systemAllowlist))
	for _, entry := range systemAllowlist {
		allowed[entry] = struct{}{}
	}
	var problems []error
	for _, path := range paths {
		scoped, upstreamScoped, err := periapsis.ParseUpstreamSecretStorePath(path)
		if err != nil {
			problems = append(problems, fmt.Errorf("argoworkflow: %w", err))
			continue
		}
		if upstreamScoped {
			if err := scoped.Authorize(org, repo); err != nil {
				problems = append(problems, fmt.Errorf("argoworkflow: %w", err))
			}
			continue
		}
		// System-namespace paths are release-only. A CI pipeline declaring one
		// would need the release SA to read it, which is the trust-tier collapse
		// this gate exists to prevent.
		if trigger == periapsis.TriggerCI {
			problems = append(problems, fmt.Errorf(
				"argoworkflow: secret store path %q is a system-namespace path; "+
					"CI pipelines may only declare upstream-scoped paths (%s<org>/<repo>/<secret>)",
				path, periapsis.UpstreamSecretPathPrefix))
			continue
		}
		if _, ok := allowed[path]; !ok {
			problems = append(problems, fmt.Errorf("argoworkflow: secret store path %q is not in the administrator allowlist", path))
		}
	}
	return errors.Join(problems...)
}
