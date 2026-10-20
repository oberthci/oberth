package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

// ReleasePreflighter observes declarations and public capabilities only.
type ReleasePreflighter interface {
	PreflightRelease(context.Context, JobRequest, []byte) ([]string, error)
}

const releasePreflightFile = ".oberth/release.yaml"
const releasePreflightLimit = 1 << 20

func releasePreflight(ctx context.Context, git vmrunner.PolicyGit, checker ReleasePreflighter, request JobRequest, base string, defaultBase bool) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	repo := request.UpstreamName + "/" + request.UpstreamOrg + "/" + request.Repository.Name
	sha := request.Run.TestedSHA
	if sha == "" {
		sha = request.Run.SHA
	}
	source, exists, err := git.ReadOptionalBlob(ctx, repo, sha, releasePreflightFile, releasePreflightLimit)
	if err != nil || !exists {
		return nil, err
	}
	var previous []byte
	if defaultBase {
		_, previous, _, err = git.ReadDefaultBlob(ctx, repo, releasePreflightFile, releasePreflightLimit)
	} else if base != "" && base != zeroOID {
		previous, _, err = git.ReadOptionalBlob(ctx, repo, base, releasePreflightFile, releasePreflightLimit)
	}
	if err != nil {
		return nil, err
	}
	if bytes.Equal(source, previous) {
		return nil, nil
	}
	return checker.PreflightRelease(ctx, request, source)
}

func (scheduler *Scheduler) checkReleasePreflight(ctx context.Context, request JobRequest, output io.Writer) error {
	checker, ok := scheduler.jobs.(ReleasePreflighter)
	if !ok || request.Run.Release {
		return nil
	}
	problems, err := releasePreflight(ctx, scheduler.git, checker, request, request.Run.BaseSHA, request.Run.BaseSHA == "")
	if err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) == 0 {
		_, err = fmt.Fprintln(output, "[release-preflight/check] passed (or release.yaml unchanged)")
		return err
	}
	for _, problem := range problems {
		if _, err := fmt.Fprintf(output, "[release-preflight/check] warning: %s\n", problem); err != nil {
			return err
		}
	}
	if request.Run.Trigger == "promotion" {
		return fmt.Errorf("release preflight: %s", strings.Join(problems, "; "))
	}
	return nil
}

func (service *API) checkPromotionRelease(ctx context.Context, repository model.Repository, sha, base string) error {
	if service.releasePreflight == nil {
		return nil
	}
	git, ok := service.git.(vmrunner.PolicyGit)
	if !ok {
		return fmt.Errorf("release preflight Git reader unavailable")
	}
	resolver, ok := service.runs.(upstreamResolver)
	if !ok {
		return fmt.Errorf("release preflight upstream resolver unavailable")
	}
	upstream, err := resolver.Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return err
	}
	request := JobRequest{Repository: repository, UpstreamName: upstream.Name, UpstreamOrg: upstream.Org(), Run: model.Run{SHA: sha, TestedSHA: sha, RefKind: model.RefBranch, Trigger: "promotion"}}
	problems, err := releasePreflight(ctx, git, service.releasePreflight, request, base, false)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("release preflight: %s", strings.Join(problems, "; "))
	}
	return nil
}
