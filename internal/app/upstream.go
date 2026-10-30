// Package app contains the small adapters that compose Oberth's server
// primitives into one process.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const defaultCatalogTimeout = 5 * time.Second

type UpstreamCatalog interface {
	RepositoryByName(context.Context, string) (model.Repository, error)
	Upstream(context.Context, int64) (model.Upstream, error)
	ListUpstreams(context.Context) ([]model.Upstream, error)
}

// Upstreams maps client-owned repository names onto an operator-owned Forge
// base URL. An unknown repository can be discovered only when one upstream is
// configured; with multiple upstreams the mapping must already be durable.
type Upstreams struct {
	Catalog UpstreamCatalog
	Timeout time.Duration
}

// Remote resolves a repository input (bare name or org-qualified "org/repo")
// to the full upstream Git remote URL. When an org prefix is provided, it is
// matched against the last path component of each upstream's base URL.
func (upstreams Upstreams) Remote(input string) (string, error) {
	if upstreams.Catalog == nil {
		return "", errors.New("app: upstream catalog is required")
	}
	upstreamName, org, repositoryName, err := gitcache.ParseRepoPath(input)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), upstreams.timeout())
	defer cancel()
	if err := upstreams.RequireActive(ctx, input); err != nil {
		return "", err
	}
	upstream, err := upstreams.selectUpstream(ctx, upstreamName, org, repositoryName)
	if err != nil {
		return "", err
	}
	remote := joinUpstream(upstream.BaseURL, repositoryName)
	if err := gitcache.ValidateUpstream(remote); err != nil {
		return "", fmt.Errorf("app: resolve upstream %s: %w", upstream.Name, err)
	}
	return remote, nil
}

// DiscoverRepository resolves a repository input (bare name or org-qualified)
// to a RepositorySpec suitable for initial catalog registration.
func (upstreams Upstreams) DiscoverRepository(ctx context.Context, input string) (model.RepositorySpec, error) {
	if upstreams.Catalog == nil {
		return model.RepositorySpec{}, errors.New("app: upstream catalog is required")
	}
	upstreamName, org, repositoryName, err := gitcache.ParseRepoPath(input)
	if err != nil {
		return model.RepositorySpec{}, err
	}
	upstream, err := upstreams.selectUpstream(ctx, upstreamName, org, repositoryName)
	if err != nil {
		return model.RepositorySpec{}, err
	}
	return model.RepositorySpec{Name: repositoryName, UpstreamID: upstream.ID}, nil
}

func (upstreams Upstreams) selectUpstream(ctx context.Context, upstreamName, org, repositoryName string) (model.Upstream, error) {
	// The catalog lookup carries the full client context: the store resolves
	// bare, org-qualified, and upstream-qualified selectors and detects
	// bare-name ambiguity itself (issue #264). A repository registered under
	// a DIFFERENT upstream/org therefore no longer shadows this path — the
	// scoped lookup misses and resolution falls through to upstream
	// discovery below, which selects the upstream the path names.
	selector := repositoryName
	if org != "" {
		selector = org + "/" + selector
	}
	if upstreamName != "" {
		selector = upstreamName + "/" + selector
	}
	repository, err := upstreams.Catalog.RepositoryByName(ctx, selector)
	if err == nil {
		upstream, lookupErr := upstreams.Catalog.Upstream(ctx, repository.UpstreamID)
		if lookupErr != nil {
			return model.Upstream{}, fmt.Errorf("app: load repository upstream: %w", lookupErr)
		}
		// Defense in depth: the scoped lookup above can only return a
		// repository matching the provided context, but re-validate so a
		// store-layer regression cannot silently cross a trust boundary.
		if upstreamName != "" && !strings.EqualFold(upstream.Name, upstreamName) {
			return model.Upstream{}, fmt.Errorf("app: repository %s is registered under upstream %q, not %q: %w", repositoryName, upstream.Name, upstreamName, gitcache.ErrUpstreamRefused)
		}
		if org != "" && !upstreamMatchesOrg(upstream, org) {
			return model.Upstream{}, upstreamOrgMismatch(repositoryName, upstream, org)
		}
		return upstream, nil
	}
	if errors.Is(err, store.ErrAmbiguous) {
		// A bare name that exists under multiple upstreams must be refused,
		// never guessed: the store's error names the conflicting upstreams
		// and the qualified forms that disambiguate.
		return model.Upstream{}, fmt.Errorf("app: %w: %w", err, gitcache.ErrUpstreamRefused)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.Upstream{}, fmt.Errorf("app: look up repository mapping: %w", err)
	}
	values, err := upstreams.Catalog.ListUpstreams(ctx)
	if err != nil {
		return model.Upstream{}, fmt.Errorf("app: list upstreams: %w", err)
	}
	if len(values) == 0 {
		return model.Upstream{}, errors.New("app: no upstream is configured")
	}

	// When an upstream name is provided (3-segment path), match by name —
	// and the org segment must agree with that upstream's identity: a path
	// like "github/acme/repo" names an (upstream, org) pair that does
	// not exist and must refuse rather than silently resolve into the
	// upstream's real org.
	if upstreamName != "" {
		for _, u := range values {
			if strings.EqualFold(u.Name, upstreamName) {
				if org != "" && !upstreamMatchesOrg(u, org) {
					return model.Upstream{}, upstreamOrgMismatch(repositoryName, u, org)
				}
				return u, nil
			}
		}
		return model.Upstream{}, fmt.Errorf("app: no upstream registered with name %q; available: %s: %w", upstreamName, formatUpstreams(values), gitcache.ErrUpstreamRefused)
	}

	// When an org prefix is provided, match it against upstream base URLs.
	// Collect all matches; more than one is an ambiguity error.
	if org != "" {
		var matched []model.Upstream
		for _, u := range values {
			if upstreamMatchesOrg(u, org) {
				matched = append(matched, u)
			}
		}
		switch len(matched) {
		case 0:
			return model.Upstream{}, fmt.Errorf("app: no upstream registered for %q; available: %s: %w", org, formatUpstreams(values), gitcache.ErrUpstreamRefused)
		case 1:
			return matched[0], nil
		default:
			names := make([]string, len(matched))
			for i, u := range matched {
				names[i] = u.Name
			}
			return model.Upstream{}, fmt.Errorf("app: org %q matches multiple upstreams: %s", org, strings.Join(names, ", "))
		}
	}

	// No org — require exactly one upstream for implicit discovery.
	if len(values) != 1 {
		return model.Upstream{}, fmt.Errorf("app: repository %s has no mapping and %d upstreams are configured; use upstream/org/repo or org/repo format (available: %s)", repositoryName, len(values), formatUpstreams(values))
	}
	return values[0], nil
}

// upstreamMatchesOrg checks whether an upstream's org identity matches the
// given org name. For example, a base URL of "ssh://git@github.com/oberthci"
// matches org "oberthci". Clone-path matching stays case-insensitive for
// operator convenience; the registered spelling from model.Upstream.Org is
// still the single canonical identity (secret path scoping matches it
// exactly).
// upstreamOrgMismatch names the value the comparison actually used. The check
// is on the upstream's org, derived from its base URL, so reporting the
// upstream's Name instead produced "upstream \"local\", not \"local\"" whenever a
// deployment named an upstream after something other than the last segment of
// its URL: an error stating that a value is not itself, which tells the reader
// nothing about the path they should have pushed to.
func upstreamOrgMismatch(repositoryName string, upstream model.Upstream, org string) error {
	registered := upstream.Org()
	if upstream.Name != "" && !strings.EqualFold(upstream.Name, registered) {
		return fmt.Errorf("app: repository %s belongs to org %q (upstream %q), not %q: %w",
			repositoryName, registered, upstream.Name, org, gitcache.ErrUpstreamRefused)
	}
	return fmt.Errorf("app: repository %s belongs to org %q, not %q: %w", repositoryName, registered, org, gitcache.ErrUpstreamRefused)
}

func upstreamMatchesOrg(upstream model.Upstream, org string) bool {
	registered := upstream.Org()
	return registered != "" && strings.EqualFold(registered, org)
}

// formatUpstreams produces a human-readable list of available upstreams with
// their org identifiers for error messages.
func formatUpstreams(values []model.Upstream) string {
	parts := make([]string, len(values))
	for i, u := range values {
		org := extractOrgFromBase(u.BaseURL)
		if u.Name != "" {
			parts[i] = fmt.Sprintf("%s (%s)", org, u.Name)
		} else {
			parts[i] = org
		}
	}
	return strings.Join(parts, ", ")
}

func extractOrgFromBase(baseURL string) string {
	org := model.Upstream{BaseURL: baseURL}.Org()
	if org == "" {
		return "unknown"
	}
	return org
}

func (upstreams Upstreams) timeout() time.Duration {
	if upstreams.Timeout <= 0 {
		return defaultCatalogTimeout
	}
	return upstreams.Timeout
}

func joinUpstream(baseURL, repositoryName string) string {
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if filepath.IsAbs(baseURL) {
		return filepath.Join(baseURL, repositoryName+".git")
	}
	return baseURL + "/" + repositoryName + ".git"
}

func ValidateUpstreamBase(baseURL string) error {
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || strings.HasSuffix(strings.ToLower(baseURL), ".git") {
		return errors.New("app: upstream base must identify a repository namespace, not one repository")
	}
	if err := gitcache.ValidateUpstream(joinUpstream(baseURL, "oberth-probe")); err != nil {
		return fmt.Errorf("app: invalid upstream base: %w", err)
	}
	return nil
}

func UpstreamKind(baseURL string) (string, error) {
	if err := ValidateUpstreamBase(baseURL); err != nil {
		return "", err
	}
	if filepath.IsAbs(strings.TrimSpace(baseURL)) {
		return "local", nil
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("app: parse upstream base: %w", err)
	}
	return parsed.Scheme, nil
}

// QualifyInput resolves any accepted repository input (bare, org-qualified,
// or upstream-qualified) to its canonical catalog identity for git-cache
// layout purposes. The returned segments are the catalog's registered
// spellings, so every input form of one repository maps to one on-disk
// cache directory (issue #264). Unknown repositories resolve through the
// same discovery rules a push uses; a bare name that exists under multiple
// upstreams is refused with the store's disambiguation guidance.
func (upstreams Upstreams) QualifyInput(input string) (gitcache.RepoQualification, error) {
	return upstreams.QualifyInputContext(context.Background(), input)
}

// QualifyInputContext preserves catalog authority within a bounded reader call.
func (upstreams Upstreams) QualifyInputContext(ctx context.Context, input string) (gitcache.RepoQualification, error) {
	if upstreams.Catalog == nil {
		return gitcache.RepoQualification{}, errors.New("app: upstream catalog is required")
	}
	upstreamName, org, repositoryName, err := gitcache.ParseRepoPath(input)
	if err != nil {
		return gitcache.RepoQualification{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, upstreams.timeout())
	defer cancel()
	upstream, err := upstreams.selectUpstream(ctx, upstreamName, org, repositoryName)
	if err != nil {
		return gitcache.RepoQualification{}, err
	}
	qualifiedOrg := upstream.Org()
	if qualifiedOrg == "" {
		qualifiedOrg = upstream.Name
	}
	return gitcache.RepoQualification{UpstreamName: upstream.Name, Org: qualifiedOrg}, nil
}

// QualifyInScope resolves input among the named upstreams only, to its
// canonical "upstream/org/repo" path, for an upstream-scoped SSH key (#073).
// A path that resolves into scope keeps that resolution. A short path (bare
// or org/repo) that is ambiguous or lands outside the scope is tried under
// each in-scope upstream instead: a registered repository there wins, and an
// unregistered name resolves only when one in-scope upstream can hold it.
// Errors never name anything outside the scope.
func (upstreams Upstreams) QualifyInScope(input string, scope []string) (string, error) {
	upstreamName, org, repositoryName, err := gitcache.ParseRepoPath(input)
	if err != nil {
		return "", err
	}
	if qualification, err := upstreams.QualifyInput(input); err == nil && slices.Contains(scope, qualification.UpstreamName) {
		return qualification.UpstreamName + "/" + qualification.Org + "/" + repositoryName, nil
	}
	refused := fmt.Errorf("app: repository %q is not in this uplink's scope: %w", input, gitcache.ErrUpstreamRefused)
	if upstreamName != "" || upstreams.Catalog == nil {
		return "", refused
	}
	ctx, cancel := context.WithTimeout(context.Background(), upstreams.timeout())
	defer cancel()
	values, err := upstreams.Catalog.ListUpstreams(ctx)
	if err != nil {
		return "", refused
	}
	var registered, candidates []string
	for _, upstream := range values {
		if !slices.Contains(scope, upstream.Name) || org != "" && !upstreamMatchesOrg(upstream, org) {
			continue
		}
		qualifiedOrg := upstream.Org()
		if qualifiedOrg == "" {
			qualifiedOrg = upstream.Name
		}
		candidate := upstream.Name + "/" + qualifiedOrg + "/" + repositoryName
		candidates = append(candidates, candidate)
		if _, err := upstreams.Catalog.RepositoryByName(ctx, candidate); err == nil {
			registered = append(registered, candidate)
		}
	}
	switch {
	case len(registered) == 1:
		return registered[0], nil
	case len(registered) == 0 && len(candidates) == 1:
		return candidates[0], nil
	default:
		return "", refused
	}
}

// RequireActive gates upstream contact without preventing identity resolution
// needed to clean an archived repository's qualified cache directory.
func (upstreams Upstreams) RequireActive(ctx context.Context, input string) error {
	if upstreams.Catalog == nil {
		return errors.New("app: upstream catalog is required")
	}
	repository, err := upstreams.Catalog.RepositoryByName(ctx, input)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if errors.Is(err, store.ErrAmbiguous) {
		return fmt.Errorf("app: %w: %w", err, gitcache.ErrUpstreamRefused)
	}
	if err != nil {
		return err
	}
	if repository.ArchivedAt != nil {
		return fmt.Errorf("%w: %w", gitcache.ErrUpstreamRefused, store.ErrRepositoryArchived)
	}
	return nil
}
