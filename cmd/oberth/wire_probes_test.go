//go:build linux

// These helpers exercise Linux server composition.

package main

import (
	"context"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/stateport"
)

func (f *composedFixture) checkWiredProbes(t *testing.T) {
	t.Helper()
	a := serverAdministration{f.owners}
	r, err := f.owners.c.store.Repository(t.Context(), gittest.Repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	var budgets []time.Duration
	f.owners.checks.Deadline = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		budgets = append(budgets, d)
		return context.WithTimeout(ctx, d)
	}
	f.git.Git(t, f.git.Bare, "symbolic-ref", "HEAD", "refs/heads/missing-probe-head")
	if err = a.ProbeRepository(t.Context(), r); err == nil {
		t.Fatal("probe ignored missing remote HEAD")
	}
	f.git.Git(t, f.git.Bare, "symbolic-ref", "HEAD", "refs/heads/main")
	out, err := a.ProbeUpstream(t.Context(), "origin", "team/widget")
	if err != nil || !reflect.DeepEqual(out.Pending, new(false)) {
		t.Fatal("successful live probe did not clear refusal", err)
	}
	if len(budgets) != 2 || budgets[0] != 15*time.Second || budgets[1] != 15*time.Second {
		t.Fatal("probes bypassed owner deadline", budgets)
	}
	t.Run("no-repository-probed", f.checkUnprobedUpstream)
	if err = f.owners.c.store.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, r.ID); err != nil {
		t.Fatal(err)
	}
	r.Archived = true
	if err = a.ProbeRepository(t.Context(), r); err == nil || len(budgets) != 2 {
		t.Fatal("archived repository contacted forge", err)
	}
	if err = f.owners.c.store.UnarchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, r.ID); err != nil {
		t.Fatal(err)
	}
}

func (f *composedFixture) checkExecutorAccount(t *testing.T, run stateport.Run) {
	t.Helper()
	workflow, err := f.argo.ArgoprojV1alpha1().Workflows(f.config.PipelineNamespace).Get(t.Context(), run.Origin.WorkflowName, meta.GetOptions{})
	if err != nil || workflow.Spec.Executor == nil || workflow.Spec.Executor.ServiceAccountName != run.Admission.Identity.Pipeline.Name {
		t.Fatal("native executor did not use the pipeline account", err)
	}
	accounts, err := f.kube.CoreV1().ServiceAccounts(f.config.PipelineNamespace).List(t.Context(), meta.ListOptions{})
	if err != nil || len(accounts.Items) != 2 {
		t.Fatal("first build required an external executor account", accounts, err)
	}
}

func (f *composedFixture) checkUnprobedUpstream(t *testing.T) {
	t.Helper()
	a := serverAdministration{f.owners}
	u, err := f.owners.c.store.Upstream(t.Context(), "origin")
	if err != nil {
		t.Fatal(err)
	}
	u.Name, u.Org = "unchecked", "unchecked-team"
	if err = f.owners.c.store.RegisterUpstreamBinding(t.Context(), stateport.Action{Actor: "operator"}, u); err != nil {
		t.Fatal(err)
	}
	pending, err := a.ProbeUpstream(t.Context(), u.Name, "")
	if err != nil || pending.Pending == nil || !*pending.Pending || pending.DisplayReason != "No repository was checked." {
		t.Fatal("unprobed upstream was reported as verified", pending, err)
	}
}
