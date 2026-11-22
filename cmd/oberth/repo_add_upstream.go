package main

import (
	"flag"
	"net/url"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
)

func repositoryUpstream(s *flowSession, u *url.URL, name string, flags *flag.FlagSet) (string, error) {
	var page apicontract.UpstreamPage
	if err := s.invoke("upstream_list", apicontract.UpstreamListArgs{}, &page); err != nil {
		return name, err
	}
	source, existing, err := selectRepositoryUpstream(page.Upstreams, u, name)
	if err != nil {
		return source.Name, err
	}
	if !existing && !strings.Contains(strings.Trim(u.Path, "/"), "/") {
		return source.Name, publicerr.New(publicerr.InvalidInput, "A new upstream needs an organization in the URL path.", "Use a forge URL shaped like ssh://host/organization/repository.git.", nil)
	}
	repo, err := repositoryFromURL(source.Name, u)
	if err != nil {
		return source.Name, err
	}
	if err = s.checkGitBinding(repo.Upstream+"/"+repo.Organization+"/"+repo.Name, "repo add"); err != nil {
		return source.Name, err
	}
	if existing && (name != "" || *flagValue[string](flags, "expected-host-fingerprint") == "" && !*flagValue[bool](flags, "repin")) {
		return source.Name, nil
	}
	return source.Name, registerUpstream(s, u, source, flags)
}

func selectRepositoryUpstream(sources []apicontract.Upstream, u *url.URL, name string) (apicontract.Upstream, bool, error) {
	if source, exists := organizationUpstream(sources, u); exists {
		if err := requireUpstreamURL(source, u, false); err != nil {
			return source, true, err
		}
		if name != "" && name != source.Name {
			return source, true, wrongRepositoryUpstream(source.Name)
		}
		return source, true, nil
	}
	for _, source := range sources {
		if source.Name == name || sameForgeSpelling(source.BaseURL, u) {
			return source, true, requireUpstreamURL(source, u, false)
		}
	}
	if name == "" {
		if err := unmatchedUpstream(sources, u, false); err != nil {
			return apicontract.Upstream{}, false, err
		}
		name = derivedUpstreamName(u)
	}
	return apicontract.Upstream{Name: name, BaseURL: upstreamAddress(u)}, false, nil
}

func registerUpstream(s *flowSession, u *url.URL, source apicontract.Upstream, flags *flag.FlagSet) error {
	repo, err := repositoryFromURL(source.Name, u)
	if err != nil {
		return err
	}
	var registered apicontract.Upstream
	return s.invoke("upstream_add", apicontract.UpstreamAddArgs{Name: source.Name, BaseURL: source.BaseURL, CheckRepo: repo.Organization + "/" + repo.Name, ExpectedHostFingerprint: *flagValue[string](flags, "expected-host-fingerprint"), Repin: *flagValue[bool](flags, "repin")}, &registered)
}

func derivedUpstreamName(u *url.URL) string { return strings.ReplaceAll(u.Hostname(), ".", "-") }

func unmatchedUpstream(sources []apicontract.Upstream, u *url.URL, joined bool) error {
	if source, ok := organizationUpstream(sources, u); ok {
		return requireUpstreamURL(source, u, joined)
	}
	for _, source := range sources {
		base, err := url.Parse(source.BaseURL)
		if err == nil && base.Scheme == u.Scheme && base.Host == u.Host || sameForgeSpelling(source.BaseURL, u) {
			return requireUpstreamURL(source, u, joined)
		}
	}
	return nil
}

func repositoryFromURL(upstream string, u *url.URL) (reponame.Repository, error) {
	parts := strings.Split(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if len(parts) < 2 {
		return reponame.ParseRepository(upstream + "/" + strings.Join(parts, "/"))
	}
	return reponame.ParseRepository(upstream + "/" + strings.Join(parts[len(parts)-2:], "/"))
}

func requireUpstreamURL(source apicontract.Upstream, u *url.URL, joined bool) error {
	if sameForgeSpelling(source.BaseURL, u) {
		problem := publicerr.New(publicerr.InvalidInput, "This is the same forge under another address spelling.", "Use the registered upstream address and retry.", nil)
		problem.Context = []string{"Upstream " + source.Name + ": " + redactedForgeURL(source.BaseURL), "Supplied URL: " + u.Redacted()}
		return problem
	}
	repo, err := repositoryFromURL(source.Name, u)
	if err != nil {
		return err
	}
	if repo.Organization != source.Org {
		problem := publicerr.New(publicerr.InvalidInput, "The repository is outside the upstream organization.", outsideOrganizationRemedy(joined), nil)
		problem.Context = []string{"Upstream " + source.Name + ": " + redactedForgeURL(source.BaseURL), "Organization: " + source.Org}
		return problem
	}
	if repo.MatchesURL(source.BaseURL, u) {
		return nil
	}
	problem := publicerr.New(publicerr.InvalidInput, "This organization uses "+source.Name+".", "Use its registered forge address; an organization has one upstream.", nil)
	problem.Context = []string{"Upstream " + source.Name + ": " + redactedForgeURL(source.BaseURL), "Supplied URL: " + u.Redacted()}
	return problem
}

func redactedForgeURL(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return "(invalid URL)"
	}
	return u.Redacted()
}

func organizationUpstream(sources []apicontract.Upstream, u *url.URL) (apicontract.Upstream, bool) {
	for _, source := range sources {
		repo, err := repositoryFromURL(source.Name, u)
		if err == nil && repo.Organization == source.Org {
			return source, true
		}
	}
	return apicontract.Upstream{}, false
}

func outsideOrganizationRemedy(joined bool) string {
	if joined {
		return "Ask an administrator to add an upstream for this organization."
	}
	return "Pass --upstream <new-name> to add an upstream for this organization."
}

func wrongRepositoryUpstream(name string) error {
	return publicerr.New(publicerr.InvalidInput, "The forge URL matches another upstream.", "Pass --upstream "+name+" and retry.", nil)
}
