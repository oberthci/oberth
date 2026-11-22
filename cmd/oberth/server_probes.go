package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
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
	key := app.ProbeKey{Address: u.BaseURL, Fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte(identity.PublicKey))), PinsDigest: fmt.Sprintf("%x", sha256.Sum256(pins)), Repository: r.Canonical}
	result := a.owner.checks.CheckRepository(ctx, key)
	if result.Detail != "" {
		a.logCheck(ctx, r.Canonical, result)
	}
	if result.Reason == gitport.EmptyForgeReason {
		return publicerr.New(publicerr.InvalidInput, gitport.EmptyForgeReason, gitport.EmptyForgeRemedy, nil)
	}
	unfinished := map[string]string{gitport.CloneUnfinishedReason: gitport.CloneUnfinishedRemedy, gitport.FetchUnfinishedReason: gitport.FetchUnfinishedRemedy}
	if remedy, ok := unfinished[result.Reason]; ok {
		problem := publicerr.New(publicerr.Unavailable, result.Reason, remedy, nil)
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

// logCheck puts a failed check's full cause in the server log; the caller's
// refusal names only its public reason.
func (a serverAdministration) logCheck(ctx context.Context, repository string, result app.ProbeResult) {
	if a.owner.c.diagnostic == nil {
		return
	}
	slog.New(slog.NewTextHandler(a.owner.c.diagnostic, nil)).WarnContext(ctx, "repository check failed", "repository", repository, "reason", result.Reason, "cause", result.Detail)
}

// ProbeUpstream checks the upstream's repositories, or only checkRepo when one
// is named. A failed check of a named repository returns its refusal, which
// carries the cause and remedy; that repository is not bound yet, so no later
// list check could show it.
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
	return checkEach(out, repos, func(r stateport.Repository) error { return a.ProbeRepository(ctx, r) }, checkRepo != "")
}

// checkEach checks repositories in order and stops at the first failure: the
// upstream is then pending with that failure's reason, whatever passed before.
// A named repository's failure is also returned, with its cause and remedy.
func checkEach(out apicontract.Upstream, repos []stateport.Repository, check func(stateport.Repository) error, named bool) (apicontract.Upstream, error) {
	out.DisplayReason = "No repository was checked."
	for _, repo := range repos {
		if err := check(repo); err != nil {
			out.Pending = true
			reason := upstreamProbeStatusReason(err)
			out.DisplayReason = reason
			if named {
				return out, err
			}
			return out, nil
		}
		out.Pending = false
		out.DisplayReason = "Repository is readable."
	}
	return out, nil
}

// upstreamProbeStatusReason is the upstream's line for a failed repository
// probe, one of the catalogue reasons the message guard checks: a host key
// mismatch, an archived repository, an empty forge repository and a first
// clone still running are named; anything else is a failed check.
func upstreamProbeStatusReason(err error) string {
	switch publicerr.Classify(err).Reason() {
	case gitssh.HostKeyMismatchReason:
		return gitssh.HostKeyMismatchReason
	case repositoryArchivedReason:
		return repositoryArchivedReason
	case gitport.EmptyForgeReason:
		return gitport.EmptyForgeReason
	case gitport.CloneUnfinishedReason:
		return gitport.CloneUnfinishedReason
	case gitport.FetchUnfinishedReason:
		return gitport.FetchUnfinishedReason
	}
	return repositoryProbeFailureReason
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
