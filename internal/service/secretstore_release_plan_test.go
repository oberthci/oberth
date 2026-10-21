package service

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

type releasePlanChecker struct {
	releaseChecker
	requests []JobRequest
}

func (c *releasePlanChecker) PlanRelease(_ context.Context, request JobRequest, _ []byte) (ReleasePreflightResult, error) {
	c.requests = append(c.requests, request)
	return ReleasePreflightResult{Checks: c.checks, Problems: c.problems}, nil
}

func TestSecretStorePlanIncludesReleaseIdentitiesWithoutGrants(t *testing.T) {
	service := testService(t)
	check := `step "publish": service_account="step-publish" release_wif_role="image-writer" subject="system:serviceaccount:argo:step-publish"`
	checker := &releasePlanChecker{releaseChecker: releaseChecker{checks: []string{check}, problems: []string{"missing grant"}}}
	service.releasePreflight = checker
	service.git = &releaseCheckGit{base: []byte("release document")}
	result, err := service.CallTool(context.Background(), api.Actor{Identity: "admin", Admin: true}, "secretstore_plan", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response := result.(SecretStorePlanResponse)
	if len(response.Releases) != 1 || response.Releases[0].Repo != "codeberg/acme/oberth" || len(response.Releases[0].Checks) != 1 || response.Releases[0].Checks[0] != check || !strings.Contains(response.Text, check) || !strings.Contains(response.Text, "missing grant") {
		t.Fatalf("release report: %+v", response)
	}
	if len(checker.requests) != 1 || checker.calls != 0 || response.Digest != computeSecretStorePlan(nil).Digest {
		t.Fatalf("inspection changed grants/digest or called recording preflight: %+v %+v", response, checker)
	}
	service.git = &releaseCheckGit{}
	response = computeSecretStorePlan(nil)
	service.addReleasePlans(context.Background(), &response)
	if len(response.Releases) != 0 || len(checker.requests) != 1 {
		t.Fatalf("missing release document inspected: %+v", response)
	}
}
