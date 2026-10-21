package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

// SecretStoreReleasePlan reports effective public identities from the default
// branch's release document. It does not change the policy materialization digest.
type SecretStoreReleasePlan struct {
	Repo     string   `json:"repo"`
	SHA      string   `json:"sha,omitempty"`
	Checks   []string `json:"checks,omitempty"`
	Problems []string `json:"problems,omitempty"`
}

type releasePlanner interface {
	PlanRelease(context.Context, JobRequest, []byte) (ReleasePreflightResult, error)
}

func (service *API) addReleasePlans(ctx context.Context, response *SecretStorePlanResponse) {
	planner, ok := service.releasePreflight.(releasePlanner)
	git, gitOK := service.git.(vmrunner.PolicyGit)
	resolver, resolverOK := service.runs.(upstreamResolver)
	if !ok || !gitOK || !resolverOK {
		response.Text += "\nrelease identities: unavailable (release planner or Git reader not configured)"
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var repos []model.Repository
	var err error
	if service.repositories != nil {
		repos, err = service.repositories.ListRepositories(ctx)
	} else {
		repos, err = service.runs.ListRepositories(ctx)
	}
	if err != nil {
		response.Text += fmt.Sprintf("\nrelease identities: %v", err)
		return
	}
	for _, repo := range repos {
		entry := SecretStoreReleasePlan{Repo: repo.Name}
		upstream, err := resolver.Upstream(ctx, repo.UpstreamID)
		if err == nil {
			entry.Repo = upstream.QualifiedRepo(repo.Name)
			var source []byte
			var exists bool
			entry.SHA, source, exists, err = git.ReadDefaultBlob(ctx, entry.Repo, releasePreflightFile, releasePreflightLimit)
			if err == nil && !exists {
				continue
			}
			if err == nil {
				org := upstream.Org()
				if org == "" {
					org = upstream.Name
				}
				var result ReleasePreflightResult
				result, err = planner.PlanRelease(ctx, JobRequest{
					Repository: repo, UpstreamName: upstream.Name, UpstreamOrg: org,
					Run: model.Run{SHA: entry.SHA, TestedSHA: entry.SHA, RefKind: model.RefBranch},
				}, source)
				entry.Checks, entry.Problems = result.Checks, result.Problems
			}
		}
		if err != nil {
			entry.Problems = append(entry.Problems, err.Error())
		}
		response.Releases = append(response.Releases, entry)
		response.Text += fmt.Sprintf("\nrelease repo: %s sha=%s", entry.Repo, entry.SHA)
		if len(entry.Checks) > 0 {
			response.Text += "\n  " + strings.Join(entry.Checks, "\n  ")
		}
		for _, problem := range entry.Problems {
			response.Text += "\n  warning: " + problem
		}
	}
}
