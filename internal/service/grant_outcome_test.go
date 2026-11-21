package service_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

const unsyncedWarning = "Saved, but this repository's schedules did not sync and stay paused; see oberth schedule list --repo " + repository

// A grant succeeds or fails on its own work: once saved and published it
// succeeds, and the schedule reconciliation it triggers is a separate line.
func TestSavedGrantSucceedsWhenItsSchedulesDidNotSync(t *testing.T) {
	t.Parallel()
	p := people(t)
	ctx := t.Context()
	p.publish.Unsynced = errors.New("schedule Git is unavailable")
	q := apicontract.AccessAllowArgs{Repo: repository, Workflow: ".oberth/build.yaml", Step: "build", Secret: "oberth/policy/names"}
	call, e := p.f.s.PrepareAccessAllow(ctx, adminCaller(), q)
	grant, e := execute(ctx, call, e)
	if e != nil || grant.Status != "approved" || grant.Warning != unsyncedWarning {
		t.Fatal("a saved grant reported failure or hid its schedules", grant, e)
	}
	snapshot, e := p.state.Grants(ctx, []string{repository})
	if e != nil || len(snapshot.Grants) != 1 {
		t.Fatal("grant was not saved", snapshot, e)
	}
	if !slices.Equal(p.publish.Repositories, []string{repository}) {
		t.Fatal("the grant did not publish for its own repository", p.publish.Repositories)
	}
}

func TestSavedCapabilityChangesSucceedWhenTheirSchedulesDidNotSync(t *testing.T) {
	t.Parallel()
	p := people(t)
	ctx := t.Context()
	p.publish.Unsynced = errors.New("schedule Git is unavailable")
	capability := apicontract.WorkflowCapabilityGrantArgs{Repo: repository, Capability: apicontract.WorkflowCapabilityGrantArgsCapability{Namespace: "jobs", ServiceAccount: "worker", ResourceKinds: []apicontract.WorkflowCapabilityGrantArgsCapabilityResourceKindsItem{{ApiVersion: "v1", Kind: "Pod", Actions: []string{"create"}}}}}
	granted, e := p.f.s.PrepareWorkflowCapabilityGrant(ctx, adminCaller(), capability)
	v, e := execute(ctx, granted, e)
	if e != nil || v.Warning != unsyncedWarning {
		t.Fatal("a saved capability reported failure or hid its schedules", v, e)
	}
	stored, e := p.f.s.PrepareWorkflowCapabilityGet(ctx, adminCaller(), apicontract.WorkflowCapabilityGetArgs{Repo: repository})
	if got, e := execute(ctx, stored, e); e != nil || got.Warning != "" || got.ServiceAccount != "worker" {
		t.Fatal("the warning reached the stored capability", got, e)
	}
	revoke, e := p.f.s.PrepareWorkflowCapabilityRevoke(ctx, adminCaller(), apicontract.WorkflowCapabilityRevokeArgs{Repo: repository})
	receipt, e := execute(ctx, revoke, e)
	if e != nil || receipt.State != "revoked" || receipt.Warning != unsyncedWarning {
		t.Fatal("a saved revocation reported failure or hid its schedules", receipt, e)
	}
}
