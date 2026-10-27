package service

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type routedReleaseCheckGit struct{ *releaseCheckGit }

func (g *routedReleaseCheckGit) ReadOptionalBlob(ctx context.Context, repo, sha, file string, limit int) ([]byte, bool, error) {
	if file == ".oberth/deploy.yaml" {
		return g.releaseCheckGit.ReadOptionalBlob(ctx, repo, sha, releasePreflightFile, limit)
	}
	return nil, false, nil
}
func (g *routedReleaseCheckGit) ReadDefaultBlob(ctx context.Context, repo, file string, limit int) (string, []byte, bool, error) {
	if file == ".oberth/deploy.yaml" {
		return g.releaseCheckGit.ReadDefaultBlob(ctx, repo, releasePreflightFile, limit)
	}
	return "", nil, false, nil
}

type routedReleaseChecker struct {
	releaseChecker
	files    []string
	requests []JobRequest
}

func (c *routedReleaseChecker) PreflightRelease(ctx context.Context, request JobRequest, source []byte) (ReleasePreflightResult, error) {
	c.requests = append(c.requests, request)
	return c.releaseChecker.PreflightRelease(ctx, request, source)
}

func (c *routedReleaseChecker) ReleasePreflightFiles(context.Context, JobRequest) ([]string, error) {
	return c.files, nil
}

func TestReleasePreflightIncludesAdminRoutedFiles(t *testing.T) {
	checker := &routedReleaseChecker{files: []string{releasePreflightFile, ".oberth/deploy.yaml"}, releaseChecker: releaseChecker{problems: []string{"unapproved role"}}}
	git := &routedReleaseCheckGit{releaseCheckGit: &releaseCheckGit{source: []byte("new"), base: []byte("old")}}
	request := JobRequest{Run: model.Run{SHA: strings.Repeat("a", 40)}}
	result, err := releasePreflight(t.Context(), git, checker, request, "", true)
	if err != nil || checker.calls != 1 || len(result.Problems) != 1 || !strings.Contains(result.Problems[0], ".oberth/deploy.yaml: unapproved role") {
		t.Fatalf("routed declaration bypassed preflight: %+v err=%v calls=%d", result, err, checker.calls)
	}
	if len(checker.requests) != 1 || checker.requests[0].PipelineFile != ".oberth/deploy.yaml" {
		t.Fatalf("preflight lost source-file provenance: %+v", checker.requests)
	}
	git.source = nil
	result, err = releasePreflight(t.Context(), git, checker, request, "", true)
	if err != nil || len(result.Problems) != 1 || !strings.Contains(result.Problems[0], "configured release workflow") {
		t.Fatalf("missing configured file ignored: %+v %v", result, err)
	}
}

type pinnedReleaseFilesGit struct {
	*releaseCheckGit
	defaults int
	files    []string
}

func (g *pinnedReleaseFilesGit) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	g.defaults++
	return strings.Repeat("b", 40), []byte("old"), true, nil
}
func (g *pinnedReleaseFilesGit) ReadOptionalBlob(_ context.Context, _ string, sha, file string, _ int) ([]byte, bool, error) {
	if sha == strings.Repeat("b", 40) {
		g.files = append(g.files, file)
		return []byte("old"), true, nil
	}
	return []byte("new"), true, nil
}
func TestRoutedPreflightPinsOneDefaultCommit(t *testing.T) {
	checker := &routedReleaseChecker{files: []string{releasePreflightFile, ".oberth/deploy.yaml"}}
	git := &pinnedReleaseFilesGit{releaseCheckGit: &releaseCheckGit{}}
	_, err := releasePreflight(t.Context(), git, checker, JobRequest{Run: model.Run{SHA: strings.Repeat("a", 40)}}, "", true)
	if err != nil || git.defaults != 1 || len(git.files) != 1 || git.files[0] != ".oberth/deploy.yaml" || checker.calls != 2 {
		t.Fatalf("mixed default revisions: defaults=%d files=%v calls=%d err=%v", git.defaults, git.files, checker.calls, err)
	}
}

func TestRoutedIdentityPlanCarriesFileProvenance(t *testing.T) {
	service := testService(t)
	checker := &routedReleasePlanner{releasePlanChecker: releasePlanChecker{}, files: []string{releasePreflightFile, ".oberth/deploy.yaml"}}
	service.releasePreflight = checker
	service.git = &pinnedReleaseFilesGit{releaseCheckGit: &releaseCheckGit{}}
	response := computeSecretStorePlan(nil)
	service.addReleasePlans(t.Context(), &response)
	if len(checker.requests) != 2 || checker.requests[0].PipelineFile != releasePreflightFile || checker.requests[1].PipelineFile != ".oberth/deploy.yaml" {
		t.Fatalf("identity plan lost file provenance: %+v", checker.requests)
	}
}

type routedReleasePlanner struct {
	releasePlanChecker
	files []string
}

func (c *routedReleasePlanner) ReleasePreflightFiles(context.Context, JobRequest) ([]string, error) {
	return c.files, nil
}
