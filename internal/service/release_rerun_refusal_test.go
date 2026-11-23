package service_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestReleaseRerunRefusalNamesAdministratorRule(t *testing.T) {
	f := setup(t)
	r := f.seed(stepmodel.RunFailed)
	r.Release, r.Origin.Trigger, r.Ref = true, model.TriggerTag, "refs/tags/v1.0.0"
	f.store.native.SeedRun(r)
	f.git.Resolve = func(context.Context, string, string) (gitport.Ref, error) {
		t.Fatal("refused rerun inspected Git")
		return gitport.Ref{}, nil
	}
	f.starter.Start = func(context.Context, service.StartRequest) (apicontract.Receipt, error) {
		t.Fatal("refused rerun started work")
		return apicontract.Receipt{}, nil
	}
	call, err := f.s.PrepareRunRerun(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunRerunArgs{ID: r.ID, Reason: "retry a failed release"})
	if call != nil || err == nil {
		t.Fatal("agent release rerun was prepared")
	}
	p := publicerr.Classify(err)
	if p.Code() != publicerr.Forbidden || p.Reason() != "Release reruns need an administrator." || p.Remedy() != "Ask an administrator to rerun this release, or release the next version." {
		t.Fatalf("release rule missing: %v", p)
	}
	if len(p.Reason()) > 40 || len(p.Remedy()) > 120 {
		t.Fatal("release refusal exceeds message limits")
	}
}
