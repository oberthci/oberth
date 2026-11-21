package workflowpolicy

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/reponame"
)

const (
	MaxRouteRepositories   = 256
	MaxRoutesPerRepository = 32
	MaxRoutePatternBytes   = 256
)

// ReleaseRoute is administrator policy matching a bare tag to one exact file.
type ReleaseRoute struct {
	Pattern string `json:"pattern"`
	File    string `json:"file"`
}

type ReleaseRoutes map[string][]ReleaseRoute

func (r ReleaseRoutes) Clone() ReleaseRoutes {
	if r == nil {
		return nil
	}
	copy := make(ReleaseRoutes, len(r))
	for repo, routes := range r {
		copy[repo] = slices.Clone(routes)
	}
	return copy
}

// Validate bounds startup policy and refuses duplicate or malformed patterns and
// noncanonical repositories. Repository iteration is sorted for stable refusals.
func (r ReleaseRoutes) Validate() error {
	if len(r) > MaxRouteRepositories {
		return &InvalidError{Path: "routes", reason: fmt.Sprintf("%d repositories exceed the route limit %d", len(r), MaxRouteRepositories)}
	}
	repos := make([]string, 0, len(r))
	for repo := range r {
		repos = append(repos, repo)
	}
	slices.Sort(repos)
	for _, repo := range repos {
		if _, err := reponame.ParseRepository(repo); err != nil {
			return &InvalidError{Path: "repo", reason: "Repository name is not canonical", detail: fmt.Sprintf("Repository: %q.", repo)}
		}
		if err := validateRoutes(repo, r[repo]); err != nil {
			return err
		}
	}
	return nil
}

func validateRoutes(repo string, routes []ReleaseRoute) error {
	if len(routes) == 0 || len(routes) > MaxRoutesPerRepository {
		return &InvalidError{Path: "routes", reason: fmt.Sprintf("Repository requires 1..%d routes", MaxRoutesPerRepository), detail: fmt.Sprintf("Repository: %q.", repo)}
	}
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		if err := validateRoute(repo, route); err != nil {
			return err
		}
		if seen[route.Pattern] {
			return routeError(repo, route, "pattern", "duplicate route pattern")
		}
		seen[route.Pattern] = true
	}
	return nil
}

func validateRoute(repo string, route ReleaseRoute) error {
	_, err := path.Match(route.Pattern, "")
	if err != nil || route.Pattern == "" || len(route.Pattern) > MaxRoutePatternBytes ||
		strings.HasPrefix(route.Pattern, "refs/") || strings.ContainsAny(route.Pattern, "\\\x00\r\n\t ") {
		return routeError(repo, route, "pattern", "requires a bounded bare-tag glob")
	}
	if err := ValidateFile(route.File); err != nil {
		var invalid *InvalidError
		if !errors.As(err, &invalid) {
			return err
		}
		return routeError(repo, route, "file", invalid.Reason())
	}
	return nil
}

// Resolve selects the release file for a bare tag; C3 calls it only for tag runs. One
// matching route wins; zero selects the default release file, overlap refuses.
// Validate policy once when loading configuration. The returned pattern retains
// the administrator decision for identity binding.
func (r ReleaseRoutes) Resolve(repo, tag string) (ReleaseRoute, error) {
	if tag == "" || strings.HasPrefix(tag, "refs/") {
		return ReleaseRoute{}, &InvalidError{Path: "ref", reason: "requires a bare tag name"}
	}
	if _, err := reponame.ParseRepository(repo); err != nil {
		return ReleaseRoute{}, &InvalidError{Path: "repo", reason: "requires a canonical repository"}
	}
	selected := ReleaseRoute{File: ReleaseFile}
	for _, route := range r[repo] {
		matched, err := path.Match(route.Pattern, tag)
		if err != nil {
			return ReleaseRoute{}, routeError(repo, route, "pattern", "Tag pattern is invalid")
		}
		if !matched {
			continue
		}
		if selected.Pattern != "" {
			return ReleaseRoute{}, &InvalidError{Path: "routes", reason: "Tag matches multiple release routes", detail: fmt.Sprintf("Repository %q tag %q matches patterns %q and %q.", repo, tag, selected.Pattern, route.Pattern)}
		}
		selected = route
	}
	return selected, nil
}

// Files inventories the default and routed files in declaration order, once
// each, for file-scoped preflight. Validate is required before using the policy.
func (r ReleaseRoutes) Files(repo string) []string {
	files := []string{ReleaseFile}
	for _, route := range r[repo] {
		if !slices.Contains(files, route.File) {
			files = append(files, route.File)
		}
	}
	return files
}

func routeError(repo string, route ReleaseRoute, field, reason string) error {
	return &InvalidError{Path: field, reason: reason, detail: fmt.Sprintf("Repository %q pattern %q file %q.", repo, route.Pattern, route.File)}
}
