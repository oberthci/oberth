package workflowpolicy

import (
	"path"
	"strings"
)

// SelectionRequest combines the service's release decision with route policy.
// File selects an inspection; without Repository only .oberth/ paths have a kind.
// Tag reports the trigger kind independently of the run model.
type SelectionRequest struct {
	Repository, Ref, File string
	Tag, Release          bool
}

// Select resolves the file and its role together. A verified tag release remains
// a release even when its route selects BuildFile. Unclassified submissions use
// BuildFile; file-only inspections infer a role without granting release authority.
func Select(routes ReleaseRoutes, q SelectionRequest) (string, bool, error) {
	release := q.Release && q.Tag && strings.HasPrefix(q.Ref, "refs/tags/")
	file := q.File
	if file == "" && q.Ref != "" {
		file = BuildFile
		if release {
			route, err := routes.Resolve(q.Repository, strings.TrimPrefix(q.Ref, "refs/tags/"))
			if err != nil {
				return "", false, err
			}
			file = route.File
		}
	}
	if q.Repository == "" {
		file = path.Clean(file)
		if !strings.HasPrefix(file, ".oberth/") {
			file = ""
		}
	}
	return file, release || file != "" && file != BuildFile, nil
}
