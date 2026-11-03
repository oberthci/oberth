package argojob

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/vmrunner"
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
	return len(k8svalidation.IsDNS1123Label(name)) == 0 && !slices.Contains([]string{argoworkflow.BuildFile, argoworkflow.FragmentFile, schedule.FileName, vmrunner.ContractFile}, file)
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

// Resolve requires the durable run kind and bare tag name written by AdmitRelease.
// A branch with the same spelling is never a release route.
func (routes ReleaseTagRoutes) Resolve(repo string, kind model.RefKind, ref string) (string, error) {
	route, err := routes.resolveRoute(repo, kind, ref)
	return route.File, err
}

// Preserve the exact selected pattern as well as its file for identity binding.
func (routes ReleaseTagRoutes) resolveRoute(repo string, kind model.RefKind, ref string) (ReleaseTagRoute, error) {
	if kind != model.RefTag || ref == "" || strings.HasPrefix(ref, "refs/") {
		return ReleaseTagRoute{}, errors.New("argojob: release tag dispatch requires a tag run and bare tag name")
	}
	selected := ReleaseTagRoute{File: argoworkflow.ReleaseFile}
	for _, entry := range routes[repo] {
		matched, err := path.Match(entry.Pattern, ref)
		if err != nil {
			return ReleaseTagRoute{}, err
		}
		if matched {
			if selected.Pattern != "" {
				return ReleaseTagRoute{}, fmt.Errorf("argojob: ambiguous release tag routes for %q ref %q", repo, ref)
			}
			selected = entry
		}
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
