package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
)

func TestProposedReleaseTagReachesTheReadOnlyCheckPort(t *testing.T) {
	t.Parallel()
	f := setup(t)
	called := false
	f.exec.InspectFunc = func(_ context.Context, q service.CheckRequest) (apicontract.RunCheckResult, error) {
		called = true
		if q.ReleaseTag != "v1.2.3" || q.Request.Ref != strings.Repeat("a", 40) || q.Request.Repo != repository {
			t.Fatal("proposed release identity was lost", q)
		}
		return apicontract.RunCheckResult{OK: true, ReleaseTag: q.ReleaseTag}, nil
	}
	call, err := f.s.PrepareRunCheck(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunCheckArgs{Repo: repository, Ref: strings.Repeat("a", 40), ReleaseTag: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := call(t.Context())
	if err != nil || !called || result.ReleaseTag != "v1.2.3" {
		t.Fatal("release check did not reach wiring", result, err)
	}
	if _, err := f.s.PrepareRunCheck(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunCheckArgs{Repo: repository, ReleaseTag: "ordinary-tag"}); err == nil {
		t.Fatal("non-release tag passed release preflight")
	}
}
