package argojob

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// ReleaseTagRoutes is startup-pinned administrator policy, never repository YAML.
// A selected document retains its own normal size/resource declarations.
type ReleaseTagRoutes map[string][]ReleaseTagRoute

type ReleaseTagRoute struct {
	Pattern string `json:"pattern"`
	File    string `json:"file"`
}

func (routes ReleaseTagRoutes) Clone() ReleaseTagRoutes {
	if routes == nil {
		return nil
	}
	copy := make(ReleaseTagRoutes, len(routes))
	for repo, entries := range routes {
		copy[repo] = slices.Clone(entries)
	}
	return copy
}

func validReleaseRouteFile(file string) bool {
	if path.Clean(file) != file || !strings.HasPrefix(file, ".oberth/") || strings.Count(file, "/") != 1 || !strings.HasSuffix(file, ".yaml") {
		return false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(file, ".oberth/"), ".yaml")
	return len(k8svalidation.IsDNS1123Label(name)) == 0 && file != argoworkflow.BuildFile && file != argoworkflow.FragmentFile && file != ".oberth/schedule.yaml"
}

func (routes ReleaseTagRoutes) Validate() error {
	if len(routes) > 256 {
		return errors.New("argojob: release tag routes accept at most 256 repositories")
	}
	for repo, entries := range routes {
		upstream, org, name, err := gitcache.ParseRepoPath(repo)
		if err != nil || upstream == "" || gitcache.ValidateUpstreamName(upstream) != nil || repo != upstream+"/"+org+"/"+name {
			return fmt.Errorf("argojob: release tag routes require canonical upstream/org/repo: %q", repo)
		}
		if len(entries) == 0 || len(entries) > 32 {
			return fmt.Errorf("argojob: release tag routes require 1..32 entries for %q", repo)
		}
		seen := map[string]bool{}
		for _, entry := range entries {
			_, err := path.Match(entry.Pattern, "")
			if err != nil || entry.Pattern == "" || len(entry.Pattern) > 256 || strings.ContainsAny(entry.Pattern, "\\\x00\r\n\t ") || strings.HasPrefix(entry.Pattern, "refs/") || seen[entry.Pattern] || !validReleaseRouteFile(entry.File) {
				return fmt.Errorf("argojob: invalid or duplicate release tag route for %q: pattern=%q file=%q", repo, entry.Pattern, entry.File)
			}
			seen[entry.Pattern] = true
		}
	}
	return nil
}

// Resolve requires an admitted tag's full ref, not a branch name that happens
// to match. Multiple matches are refused, never resolved by list order.
func (routes ReleaseTagRoutes) Resolve(repo, ref string) (string, error) {
	if !strings.HasPrefix(ref, "refs/tags/") || len(ref) == len("refs/tags/") {
		return "", errors.New("argojob: release tag dispatch requires refs/tags/<tag>")
	}
	selected := ""
	for _, entry := range routes[repo] {
		matched, err := path.Match(entry.Pattern, strings.TrimPrefix(ref, "refs/tags/"))
		if err != nil {
			return "", err
		}
		if matched {
			if selected != "" {
				return "", fmt.Errorf("argojob: ambiguous release tag routes for %q ref %q", repo, ref)
			}
			selected = entry.File
		}
	}
	if selected == "" {
		selected = argoworkflow.ReleaseFile
	}
	return selected, nil
}

// Files is the complete bounded preflight inventory, not pattern expansion.
func (routes ReleaseTagRoutes) Files(repo string) []string {
	files := []string{argoworkflow.ReleaseFile}
	for _, entry := range routes[repo] {
		if !slices.Contains(files, entry.File) {
			files = append(files, entry.File)
		}
	}
	return files
}
