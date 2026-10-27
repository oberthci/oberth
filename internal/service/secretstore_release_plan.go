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
			org := upstream.Org()
			if org == "" {
				org = upstream.Name
			}
			request := JobRequest{Repository: repo, UpstreamName: upstream.Name, UpstreamOrg: org, Run: model.Run{RefKind: model.RefBranch}}
			var files []string
			files, err = releasePreflightFiles(ctx, planner, request)
			found := false
			for _, file := range files {
				if err != nil {
					break
				}
				var source []byte
				var exists bool
				if entry.SHA == "" {
					entry.SHA, source, exists, err = git.ReadDefaultBlob(ctx, entry.Repo, file, releasePreflightLimit)
				} else {
					source, exists, err = git.ReadOptionalBlob(ctx, entry.Repo, entry.SHA, file, releasePreflightLimit)
				}
				if err != nil {
					break
				}
				if !exists {
					if file != releasePreflightFile {
						entry.Problems = append(entry.Problems, fmt.Sprintf("configured release workflow %q is missing", file))
						found = true
					}
					continue
				}
				found = true
				request.Run.SHA, request.Run.TestedSHA = entry.SHA, entry.SHA
				var result ReleasePreflightResult
				request.PipelineFile = file
				result, err = planner.PlanRelease(ctx, request, source)
				for _, check := range result.Checks {
					if file != releasePreflightFile {
						check = file + ": " + check
					}
					entry.Checks = append(entry.Checks, check)
				}
				for _, problem := range result.Problems {
					if file != releasePreflightFile {
						problem = file + ": " + problem
					}
					entry.Problems = append(entry.Problems, problem)
				}
			}
			if err == nil && !found {
				continue
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
