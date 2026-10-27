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
	PreflightRelease(context.Context, JobRequest, []byte) (ReleasePreflightResult, error)
}

// ReleasePreflightResult separates capability failures from public identity advice.
type ReleasePreflightResult struct {
	Problems []string
	Checks   []string
}

const releasePreflightFile = ".oberth/release.yaml"
const releasePreflightLimit = 1 << 20

func releasePreflight(ctx context.Context, git vmrunner.PolicyGit, checker ReleasePreflighter, request JobRequest, base string, defaultBase bool) (ReleasePreflightResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	repo := request.UpstreamName + "/" + request.UpstreamOrg + "/" + request.Repository.Name
	sha := request.Run.TestedSHA
	if sha == "" {
		sha = request.Run.SHA
	}
	files, err := releasePreflightFiles(ctx, checker, request)
	if err != nil {
		return ReleasePreflightResult{}, err
	}
	combined := ReleasePreflightResult{}
	defaultSHA := ""
	for _, file := range files {
		source, exists, err := git.ReadOptionalBlob(ctx, repo, sha, file, releasePreflightLimit)
		if err != nil {
			return combined, err
		}
		if !exists {
			if file != releasePreflightFile {
				combined.Problems = append(combined.Problems, fmt.Sprintf("configured release workflow %q is missing", file))
			}
			continue
		}
		var previous []byte
		if defaultBase {
			if defaultSHA == "" {
				defaultSHA, previous, _, err = git.ReadDefaultBlob(ctx, repo, file, releasePreflightLimit)
			} else {
				previous, _, err = git.ReadOptionalBlob(ctx, repo, defaultSHA, file, releasePreflightLimit)
			}
		} else if base != "" && base != zeroOID {
			previous, _, err = git.ReadOptionalBlob(ctx, repo, base, file, releasePreflightLimit)
		}
		if err != nil {
			return combined, err
		}
		if bytes.Equal(source, previous) {
			continue
		}
		result, err := checker.PreflightRelease(ctx, request, source)
		if err != nil {
			return combined, err
		}
		for _, problem := range result.Problems {
			if file != releasePreflightFile {
				problem = file + ": " + problem
			}
			combined.Problems = append(combined.Problems, problem)
		}
		for _, check := range result.Checks {
			if file != releasePreflightFile {
				check = file + ": " + check
			}
			combined.Checks = append(combined.Checks, check)
		}
	}
	return combined, nil
}

func (scheduler *Scheduler) checkReleasePreflight(ctx context.Context, request JobRequest, output io.Writer) error {
	checker, ok := scheduler.jobs.(ReleasePreflighter)
	if !ok || request.Run.Release {
		return nil
	}
	result, err := releasePreflight(ctx, scheduler.git, checker, request, request.Run.BaseSHA, request.Run.BaseSHA == "")
	if err != nil {
		result.Problems = append(result.Problems, err.Error())
	}
	for _, check := range result.Checks {
		if _, err := fmt.Fprintf(output, "[release-preflight/check] %s\n", check); err != nil {
			return err
		}
	}
	if len(result.Problems) == 0 {
		_, err = fmt.Fprintln(output, "[release-preflight/check] passed (or release.yaml unchanged)")
		return err
	}
	for _, problem := range result.Problems {
		if _, err := fmt.Fprintf(output, "[release-preflight/check] warning: %s\n", problem); err != nil {
			return err
		}
	}
	if request.Run.Trigger == "promotion" {
		return fmt.Errorf("release preflight: %s", strings.Join(result.Problems, "; "))
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
	result, err := releasePreflight(ctx, git, service.releasePreflight, request, base, false)
	if err != nil {
		return err
	}
	if len(result.Problems) > 0 {
		return fmt.Errorf("release preflight: %s", strings.Join(result.Problems, "; "))
	}
	return nil
}

// An optional engine capability extends preflight to administrator-pinned tag
// documents. Legacy engines keep exactly their default release file.
func releasePreflightFiles(ctx context.Context, checker any, request JobRequest) ([]string, error) {
	if lister, ok := checker.(interface {
		ReleasePreflightFiles(context.Context, JobRequest) ([]string, error)
	}); ok {
		return lister.ReleasePreflightFiles(ctx, request)
	}
	return []string{releasePreflightFile}, nil
}
