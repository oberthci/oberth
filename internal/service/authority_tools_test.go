package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestGrantKindsAndCapabilityRefusals(t *testing.T) {
	t.Parallel()
	p := people(t)
	ctx := t.Context()
	for _, kind := range []string{"store_path", "secret", "configmap", "image_pull_secret", "identity"} {
		q := apicontract.AccessAllowArgs{Repo: repository, Kind: kind, Workflow: ".oberth/build.yaml", Step: "build", Secret: "same/path"}
		if kind == "identity" {
			q.Secret, q.Audience = "identity", "same/path"
		}
		call, e := p.f.s.PrepareAccessAllow(ctx, adminCaller(), q)
		if _, e = execute(ctx, call, e); e != nil {
			t.Fatal(kind, e)
		}
	}
	snapshot, e := p.state.Grants(ctx, nil)
	if e != nil || len(snapshot.Grants) != 5 || len(p.publish.Snapshot.Grants) != 5 {
		t.Fatal("grant kinds collapsed", snapshot, e)
	}
	for _, q := range []apicontract.AccessAllowArgs{{Repo: repository, Kind: "unknown"}, {Repo: repository, Workflow: "../escape", Step: "build", Secret: "path"}, {Repo: repository, Workflow: ".oberth/build.yaml", Secret: "path"}, {Repo: repository, Kind: "pvc", Claim: "claim", Tier: "other"}} {
		if _, e = p.f.s.PrepareAccessAllow(ctx, adminCaller(), q); e == nil {
			t.Fatal("invalid grant accepted", q)
		}
	}
	adminList, e := p.f.s.PrepareAdminAccessList(ctx, adminCaller(), apicontract.AdminAccessListArgs{})
	material, e := execute(ctx, adminList, e)
	if e != nil || len(material.Grants) != 1 {
		t.Fatal("installer received grants other than store paths", material, e)
	}
	capabilityRefusals(t, p)
}
func capabilityRefusals(t *testing.T, p *peopleFixture) {
	q := apicontract.WorkflowCapabilityGrantArgs{Repo: repository, Capability: apicontract.WorkflowCapabilityGrantArgsCapability{Namespace: "jobs", ServiceAccount: "worker", ResourceKinds: []apicontract.WorkflowCapabilityGrantArgsCapabilityResourceKindsItem{{ApiVersion: "v1", Kind: "Pod", Actions: []string{"create"}}}}}
	for _, namespace := range []string{"oberth", "oberth-system"} {
		bad := q
		bad.Capability.Namespace = namespace
		if _, e := p.f.s.PrepareWorkflowCapabilityGrant(t.Context(), adminCaller(), bad); e == nil {
			t.Fatal("server namespace granted", namespace)
		}
	}
	q.Capability.ResourceKinds[0].Kind = "ClusterRole"
	if _, e := p.f.s.PrepareWorkflowCapabilityGrant(t.Context(), adminCaller(), q); e == nil {
		t.Fatal("cluster scoped kind granted")
	}
	q.Capability.ResourceKinds[0].Kind = "Pod"
	p.cluster.Validate = func(context.Context, apicontract.Capability) error { return errors.New("admission capability refused") }
	q.Capability.ResourceKinds[0].Actions = []string{"*"}
	if _, e := p.f.s.PrepareWorkflowCapabilityGrant(t.Context(), adminCaller(), q); e == nil {
		t.Fatal("wildcard operation granted")
	}
	p.cluster.Validate = nil
	q.Capability.ResourceKinds[0].Actions = []string{"create"}
	p.cluster.Lookup = func(context.Context, string, string) (bool, error) { return false, errors.New("discovery unavailable") }
	if _, e := p.f.s.PrepareWorkflowCapabilityGrant(t.Context(), adminCaller(), q); e == nil {
		t.Fatal("discovery error widened authority")
	}
	p.cluster.Lookup = nil
	call, e := p.f.s.PrepareWorkflowCapabilityGrant(t.Context(), adminCaller(), q)
	if _, e = execute(t.Context(), call, e); e != nil {
		t.Fatal(e)
	}
	get, e := p.f.s.PrepareWorkflowCapabilityGet(t.Context(), workerCaller(), apicontract.WorkflowCapabilityGetArgs{Repo: repository})
	got, e := execute(t.Context(), get, e)
	if e != nil || got.Namespace != "jobs" || len(got.ResourceKinds) != 1 {
		t.Fatal(got, e)
	}
}
func TestGrantRollbackDoesNotPublishAuthority(t *testing.T) {
	t.Parallel()
	p := people(t)
	failure := errors.New("audit failed")
	p.publish.Fence = func(context.Context, func(context.Context) (stateport.AuthoritySnapshot, error)) error {
		return failure
	}
	call, e := p.f.s.PrepareAccessAllow(t.Context(), adminCaller(), apicontract.AccessAllowArgs{Repo: repository, Workflow: ".oberth/build.yaml", Step: "build", Secret: "path"})
	if _, e = execute(t.Context(), call, e); !errors.Is(e, failure) {
		t.Fatal(e)
	}
	snapshot, err := p.state.Grants(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.publish.Snapshot.Grants) != 0 || len(snapshot.Grants) != 0 {
		t.Fatal("failed audit published authority")
	}

}
func TestEnrollmentRefusalsAndAtomicRegistration(t *testing.T) {
	t.Parallel()
	p := people(t)
	ctx := t.Context()
	csr, key := publicKeys(t)
	for _, q := range []apicontract.UplinkAddArgs{{Identity: "bad\nidentity", SshPublicKey: key}, {Identity: "good", SshPublicKey: "private key"}, {Identity: "good", SshPublicKey: key, Scope: []string{"../bad"}}, {Identity: "good", SshPublicKey: key, CSR: "bad CSR"}, {Identity: "good", SshPublicKey: key, TTL: "8761h"}} {
		call, e := p.f.s.PrepareUplinkAdd(ctx, adminCaller(), q)
		if _, e = execute(ctx, call, e); e == nil {
			t.Fatal("invalid identity accepted", q.Identity)
		}
	}
	good := apicontract.UplinkAddArgs{Identity: "registered", SshPublicKey: key, CSR: csr, BearerDigest: apicontract.SecretDigest("test-only-bearer")}
	call, e := p.f.s.PrepareUplinkAdd(ctx, adminCaller(), good)
	v, e := execute(ctx, call, e)
	if e != nil || v.Certificate == "" {
		t.Fatal("CLI generated public inputs not sufficient", v, e)
	}
	_, secondKey := publicKeys(t)
	good.SshPublicKey = secondKey
	call, e = p.f.s.PrepareUplinkAdd(ctx, adminCaller(), good)
	result, e := execute(ctx, call, e)
	if e == nil || result.Certificate != "" {
		t.Fatal("unbound credential returned", result, e)
	}
	rows, e := p.state.IdentityKeys(ctx, "registered")
	if e != nil || len(rows) != 1 {
		t.Fatal("pin conflict left partial key", rows, e)
	}
}
func TestInvitationPublicInputsAndReservationOrder(t *testing.T) {
	t.Parallel()
	p := people(t)
	ctx := t.Context()
	csr, key := publicKeys(t)
	base := apicontract.UplinkInviteArgs{Identity: "approved", Scope: []string{"origin"}, ServerURL: "https://ci.example/mcp", SecretDigest: digestText(strings.Repeat("s", 48))}
	for _, mutate := range []func(*apicontract.UplinkInviteArgs){func(q *apicontract.UplinkInviteArgs) { q.ServerURL = "http://ci.example" }, func(q *apicontract.UplinkInviteArgs) { q.SecretDigest = "bad" }, func(q *apicontract.UplinkInviteArgs) { q.Expires = "169h" }, func(q *apicontract.UplinkInviteArgs) { q.Expires = "0h" }} {
		q := base
		mutate(&q)
		if _, e := p.f.s.PrepareUplinkInvite(ctx, adminCaller(), q); e == nil {
			t.Fatal("invalid invitation accepted", q)
		}
	}
	call, e := p.f.s.PrepareUplinkInvite(ctx, adminCaller(), base)
	inv, e := execute(ctx, call, e)
	if e != nil {
		t.Fatal(e)
	}
	p.f.c.People.Enrollment = deniedReservation{EnrollmentStore: p.state, Invitation: inv}
	p.f.restart(t)
	if _, e = p.f.s.Redeem(ctx, inv.ID, model.InviteRedemption{Secret: strings.Repeat("s", 48), CSR: csr, SSHPublicKey: key, ServerURL: inv.ServerURL, CAPin: inv.CAPin}); e == nil || len(p.issuer.Identities) != 0 {
		t.Fatal("failed reservation's value reached issuer", e, p.issuer.Identities)
	}
}

type deniedReservation struct {
	service.EnrollmentStore
	Invitation model.Invitation
}

func (p deniedReservation) ReserveInvitation(context.Context, stateport.InvitationReservation) (model.Invitation, error) {
	return p.Invitation, publicerr.New(publicerr.Forbidden, "invitation refused", "Use an approved invitation.", nil)
}
func TestIssuerLifetimePublicBoundaries(t *testing.T) {
	t.Parallel()
	p := people(t)
	for _, extra := range []time.Duration{0, time.Second} {
		_, key := publicKeys(t)
		q := apicontract.UplinkAddArgs{Identity: "lifetime", SshPublicKey: key, BearerDigest: apicontract.SecretDigest("test-only-bearer"), TTL: (365*24*time.Hour + extra).String()}
		call, e := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), q)
		_, e = execute(t.Context(), call, e)
		if (e != nil) != (extra > 0) {
			t.Fatal("lifetime N/N+1", extra, e)
		}
	}
}
