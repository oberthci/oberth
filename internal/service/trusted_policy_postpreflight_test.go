package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type postPreflightPolicyRefusal struct {
	*protectedFixture
	after func()
}

func (p *postPreflightPolicyRefusal) Execute(ctx context.Context, job WorkflowRequest, requirement vmrunner.Requirement, out io.Writer) error {
	if err := p.protectedFixture.Execute(ctx, job, requirement, out); err != nil {
		return err
	}
	p.after()
	return nil
}

func TestTrustedPolicyPostPreflightRefusalReleasesCapacity(t *testing.T) {
	f := newControlFixture(t)
	contract := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
	g := &policyTestGit{controlGit: f.git, candidate: contract, baseline: contract}
	f.scheduler.git = g
	f.scheduler.trusted = &postPreflightPolicyRefusal{protectedFixture: &protectedFixture{store: f.store}, after: func() {
		if f.scheduler.AdmissionState().Used == 0 {
			t.Fatal("fixture never acquired admission capacity")
		}
		g.baselineErr = errors.New("permission denied after preflight")
	}}
	waits := 0
	f.scheduler.capacityWait = func(context.Context) error { waits++; return errors.New("unexpected policy wait") }
	queued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "post-preflight-policy", Repository: f.repo, Branch: "feature/policy", SHA: strings.Repeat("c", 40), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(t.Context(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunFailed || !strings.Contains(run.Error, "permission denied after preflight") || waits != 0 || f.scheduler.AdmissionState().Used != 0 || len(f.jobs.createdCI) != 0 {
		t.Fatalf("post-preflight refusal retained capacity or ran: status=%s error=%q waits=%d admission=%#v", run.Status, run.Error, waits, f.scheduler.AdmissionState())
	}
}
