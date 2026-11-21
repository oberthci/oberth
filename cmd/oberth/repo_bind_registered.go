package main

import (
	"net/url"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func bindRegisteredRepository(s *flowSession, u *url.URL, known, upstream string) error {
	var page apicontract.RepoPage
	if err := s.invoke("repo_list", apicontract.RepoListArgs{}, &page); err != nil {
		return err
	}
	repo, found := registeredURLMatch(page, u, upstream)
	if !found {
		return missingRegisteredRepository(page, u, upstream)
	}
	if err := s.checkGitBinding(repo, "repo add"); err != nil {
		return err
	}
	if err := s.bindCheckout(repo, known); err != nil {
		return err
	}
	upstream, _, _ = strings.Cut(repo, "/")
	return s.repositoryBound(upstream, repo)
}

func registeredURLMatch(page apicontract.RepoPage, u *url.URL, upstream string) (string, bool) {
	source, found := matchingUpstream(page.Upstreams, u)
	if !found || upstream != "" && upstream != source.Name {
		return "", false
	}
	repo, _ := repositoryFromURL(source.Name, u)
	canonical := repo.Upstream + "/" + repo.Organization + "/" + repo.Name
	for _, registered := range page.Repositories {
		if registered.Repo == canonical {
			return canonical, true
		}
	}
	return "", false
}

func missingRegisteredRepository(page apicontract.RepoPage, u *url.URL, upstream string) error {
	if upstream != "" {
		if alternative, found := registeredURLMatch(page, u, ""); found {
			problem := publicerr.New(publicerr.InvalidInput, "The repository is registered under another upstream.", "Pass --upstream with the upstream name from a listed repository.", nil)
			problem.Context = []string{alternative}
			return problem
		}
		if source, found := matchingUpstream(page.Upstreams, u); found && source.Name != upstream {
			return wrongRepositoryUpstream(source.Name)
		}
		for _, source := range page.Upstreams {
			if source.Name == upstream {
				if err := requireUpstreamURL(source, u, true); err != nil {
					return err
				}
				return unavailableRepository([]apicontract.Upstream{source}, u, upstream)
			}
		}
	}
	if _, found := matchingUpstream(page.Upstreams, u); !found {
		if err := unmatchedUpstream(page.Upstreams, u, true); err != nil {
			return err
		}
	}
	return unavailableRepository(page.Upstreams, u, "")
}

func unavailableRepository(sources []apicontract.Upstream, u *url.URL, upstream string) error {
	problem := publicerr.New(publicerr.Forbidden, "Repository is not available in this uplink's scope.", "Ask an administrator to register it and check this uplink's scope.", nil)

	if source, found := matchingUpstream(sources, u); found {
		upstream = source.Name
	}
	command := "Administrator: oberth repo add " + shellArgument(u.Redacted())
	if upstream != "" {
		command += " --upstream " + shellArgument(upstream)
	}
	problem.Context = []string{command}
	return problem
}

func matchingUpstream(sources []apicontract.Upstream, u *url.URL) (apicontract.Upstream, bool) {
	source, found := organizationUpstream(sources, u)
	if !found {
		return source, false
	}
	repo, _ := repositoryFromURL(source.Name, u)
	return source, repo.MatchesURL(source.BaseURL, u)
}
