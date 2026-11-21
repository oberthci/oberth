package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/stateport"
)

const repositoryProbeFailureReason = "Repository check failed."
const repositoryArchivedReason = "Repository is archived."

type probeGit struct{ *gitcache.Cache }

func (g probeGit) RemoteRef(ctx context.Context, repository, ref string) (gitport.Ref, error) {
	if err := g.Init(ctx, repository); err != nil {
		return gitport.Ref{}, err
	}
	return g.Cache.RemoteRef(ctx, repository, ref)
}
func (a serverAdministration) ProbeRepository(ctx context.Context, r stateport.Repository) error {
	if r.Archived {
		return publicerr.New(publicerr.Forbidden, repositoryArchivedReason, "Unarchive the repository before probing it.", nil)
	}
	u, err := a.owner.c.store.Upstream(ctx, r.Upstream)
	if err != nil {
		return err
	}
	identity, err := a.PublicIdentity(ctx, u.KeyName)
	if err != nil {
		return err
	}
	pins, err := os.ReadFile(a.pinsPath())
	if err != nil {
		return err
	}
	key := app.ProbeKey{Address: u.BaseURL, User: "git", Fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte(identity.PublicKey))), PinsDigest: fmt.Sprintf("%x", sha256.Sum256(pins)), Repository: r.Canonical}
	result := a.owner.checks.CheckRepository(ctx, key)
	if result.Reason == gitport.EmptyForgeReason {
		return publicerr.New(publicerr.InvalidInput, gitport.EmptyForgeReason, gitport.EmptyForgeRemedy, nil)
	}
	if result.Reason == gitport.CloneUnfinishedReason {
		problem := publicerr.New(publicerr.Unavailable, gitport.CloneUnfinishedReason, gitport.CloneUnfinishedRemedy, nil)
		problem.Context = []string{"Repository: " + r.Canonical}
		return problem
	}
	if result.Detail != "" {
		reason := repositoryProbeFailureReason
		remedy := "Check the upstream address, identity and host pins, then retry."
		if result.Reason == gitport.ForgeReason {
			reason, remedy = gitport.ForgeReason, gitport.ForgeOperatorRemedy
		}
		// Only a complete public shim headline crosses the child diagnostic boundary.
		// Native Git diagnostics can contain private staging paths and remote output.
		for _, line := range strings.Split(result.Detail, "\n") {
			if strings.TrimSpace(line) == gitssh.HostKeyMismatchReason {
				reason = gitssh.HostKeyMismatchReason
			}
		}
		problem := publicerr.New(publicerr.Unavailable, reason, remedy, nil)
		problem.Context = []string{"Repository: " + r.Canonical}
		return problem
	}
	return nil
}
func (a serverAdministration) ProbeUpstream(ctx context.Context, name, checkRepo string) (apicontract.Upstream, error) {
	u, err := a.owner.c.store.Upstream(ctx, name)
	if err != nil {
		return apicontract.Upstream{}, err
	}
	identity, err := a.PublicIdentity(ctx, u.KeyName)
	if err != nil {
		return apicontract.Upstream{}, err
	}
	out := apicontract.Upstream{Name: u.Name, Org: u.Org, BaseURL: u.BaseURL, PublicKey: identity.PublicKey, Pending: true}
	repos, err := a.probeRepositories(ctx, u, checkRepo)
	if err != nil {
		return out, err
	}
	out.DisplayReason = "No repository was checked."
	for _, repo := range repos {
		if err = a.ProbeRepository(ctx, repo); err != nil {
			out.Pending = true
			switch publicerr.Classify(err).Reason() {
			case gitssh.HostKeyMismatchReason:
				out.DisplayReason = gitssh.HostKeyMismatchReason
			case repositoryArchivedReason:
				out.DisplayReason = repositoryArchivedReason
			default:
				out.DisplayReason = repositoryProbeFailureReason
			}
			return out, nil
		}
		out.Pending = false
		out.DisplayReason = "Repository is readable."
	}
	return out, nil
}
func (a serverAdministration) probeRepositories(ctx context.Context, u stateport.Upstream, checkRepo string) ([]stateport.Repository, error) {
	if checkRepo != "" {
		canonical := u.Name + "/" + checkRepo
		parsed, err := reponame.ParseRepository(canonical)
		if err != nil || parsed.Organization != u.Org {
			return nil, probeScopeError(err)
		}
		r, err := a.owner.c.store.Repository(ctx, canonical, nil)
		if missingStore(err) {
			return []stateport.Repository{{Canonical: canonical, Upstream: u.Name, Name: parsed.Name}}, nil
		}
		return []stateport.Repository{r}, err
	}
	rows, err := a.owner.repositories(ctx, nil)
	selected := []stateport.Repository{}
	for _, r := range rows {
		if r.Upstream == u.Name {
			selected = append(selected, r)
		}
	}
	return selected, err
}
