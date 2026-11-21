package service_test

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/actor"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// Covers: REQ-C2-001
func TestValidatedEnrollmentAndIdentityContract(t *testing.T) {
	t.Parallel()
	p := people(t)
	ctx := t.Context()
	csr, key := publicKeys(t)
	secret := strings.Repeat("s", 48)
	q := apicontract.UplinkInviteArgs{Identity: "approved", Scope: []string{"origin"}, ServerURL: "https://ci.example/mcp", SecretDigest: digestText(secret)}
	call, e := p.f.s.PrepareUplinkInvite(ctx, adminCaller(), q)
	inv, e := execute(ctx, call, e)
	if e != nil {
		t.Fatal(e)
	}
	bad := model.InviteRedemption{Secret: strings.Repeat("x", 48), CSR: "bad CSR", SSHPublicKey: key, ServerURL: inv.ServerURL, CAPin: inv.CAPin}
	if _, e = p.f.s.Redeem(ctx, inv.ID, bad); e == nil || len(p.issuer.Identities) != 0 {
		t.Fatal("bad invitation reached issuer", e)
	}
	bad.Secret = secret
	bad.CSR = csr
	result, e := p.f.s.Redeem(ctx, inv.ID, bad)
	if e != nil {
		t.Fatal(e)
	}
	cert := assertEnrollmentIdentity(t, result)
	actor, e := p.state.AuthenticateCertificate(ctx, 0, base64.StdEncoding.EncodeToString(cert.RawSubjectPublicKeyInfo))
	if e != nil || actor.Identity != "approved" {
		t.Fatal("issued certificate was not bound", actor, e)
	}
	p.f.store.Actors[actor.Fingerprint] = actor
	assertRenewalIdentity(t, p, actor, csr)
	if _, e = p.f.s.Redeem(ctx, inv.ID, bad); e == nil {
		t.Fatal("consumed invitation replayed")
	}

	actor.AuthMethod = "bearer"
	p.f.store.Actors[actor.Fingerprint] = actor
	if _, e = p.f.s.PrepareClientRenew(ctx, apicontract.Caller{Actor: actor}, apicontract.ClientRenewArgs{CSR: csr}); e == nil {
		t.Fatal("bearer credential renewed a certificate")
	}
}
func digestText(v string) string { sum := sha256.Sum256([]byte(v)); return fmt.Sprintf("%x", sum) }

// Covers: REQ-C2-002
func TestAuditedAdministrativeEffectsContract(t *testing.T) {
	t.Parallel()
	p := people(t)
	ctx := t.Context()
	failure := errors.New("audit unavailable")
	effects := 0
	p.f.c.People.Administration = &testAdministration{Key: func(context.Context, apicontract.UpstreamProvideKeyArgs) (apicontract.Upstream, error) {
		effects++
		return apicontract.Upstream{Name: "origin"}, nil
	}}
	p.admin.Fail = failure
	p.f.c.People.Admin = p.admin
	p.f.restart(t)
	call, e := p.f.s.PrepareUpstreamProvideKey(ctx, adminCaller(), apicontract.UpstreamProvideKeyArgs{Name: "origin", PrivateKey: "private input"})
	_, e = execute(ctx, call, e)
	if !errors.Is(e, failure) || effects != 0 {
		t.Fatal("key effect preceded audit", e, effects)
	}
	p.f.c.People.Backup = snapshotFunc(func(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error) {
		effects++
		return stateport.BackupReport{}, nil
	})
	p.f.restart(t)
	if _, e = p.f.s.Backup(ctx, admin(), closeWriter{}); !errors.Is(e, failure) || effects != 0 {
		t.Fatal("snapshot preceded audit", e, effects)
	}
	assertBackupSlot(t, p)
	records, _, e := p.state.Audit(ctx, 0, 200)
	if e != nil {
		t.Fatal(e)
	}
	actions := []string{}
	for _, v := range records {
		actions = append(actions, v.Action)
		if strings.Contains(v.Details, "private input") {
			t.Fatal("private key entered audit")
		}
	}
	if !slices.Contains(actions, "backup.started") || !slices.Contains(actions, "backup.completed") {
		t.Fatal("missing backup audits", actions)
	}
}

// Covers: REQ-C2-009
func TestAtomicGrantPublicationContract(t *testing.T) {
	t.Parallel()
	p := people(t)
	q := apicontract.AccessAllowArgs{Repo: repository, Workflow: ".oberth/build.yaml", Step: "build", Secret: "public/key"}
	assertFencedGrant(t, p, q)
	assertRevocationSurvivesFailedPublication(t, p, q)
}
func assertFencedGrant(t *testing.T, p *peopleFixture, q apicontract.AccessAllowArgs) {
	t.Helper()
	ctx := t.Context()
	p.publish.Fence = func(ctx context.Context, apply func(context.Context) (stateport.AuthoritySnapshot, error)) error {
		before, e := p.state.Grants(ctx, nil)
		if e != nil || len(before.Grants) != 0 {
			t.Fatal("grant committed before fence", before, e)
		}
		after, e := apply(ctx)
		if e != nil {
			return e
		}
		if after.Revision != 1 || len(after.Grants) != 1 {
			t.Fatal("incomplete generation", after)
		}
		p.publish.Snapshot = after
		return nil
	}
	call, e := p.f.s.PrepareAccessAllow(ctx, adminCaller(), q)
	g, e := execute(ctx, call, e)
	if e != nil || g.Status != "approved" {
		t.Fatal(g, e)
	}
	if p.publish.Snapshot.Revision != 1 {
		t.Fatal("grant bypassed publication fence")
	}
	audits, e := p.state.AdministrativeAudit(ctx, nil, stateport.AdministrativeFilter{Limit: 200, Action: "authority.changed"})
	if e != nil {
		t.Fatal(e)
	}
	if len(audits) != 1 || audits[0].Action != "authority.changed" {
		t.Fatal("grant/audit not atomic", audits)
	}

}
func assertRevocationSurvivesFailedPublication(t *testing.T, p *peopleFixture, q apicontract.AccessAllowArgs) {
	t.Helper()
	attachRevokeStore(t, p)
	ctx := t.Context()

	p.publish.Fence = func(context.Context, func(context.Context) (stateport.AuthoritySnapshot, error)) error {
		return errors.New("invalid unrelated grant")
	}
	revoke, e := p.f.s.PrepareAccessRevoke(ctx, adminCaller(), apicontract.AccessRevokeArgs{Repo: q.Repo, Workflow: q.Workflow, Step: q.Step, Secret: q.Secret})
	receipt, e := execute(ctx, revoke, e)
	if e != nil || !strings.Contains(receipt.Warning, "Retry the grant or revocation") {
		t.Fatal("revocation was lost through failed fence", receipt, e)
	}
	assertExactGrantRevoked(t, p)
}
func assertExactGrantRevoked(t *testing.T, p *peopleFixture) {
	t.Helper()
	ctx := t.Context()
	snapshot, e := p.state.Grants(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	revoked := false
	for _, v := range snapshot.Grants {
		if v.Kind == stateport.GrantStorePath {
			revoked = v.Revoke
		}
	}
	if !revoked {
		t.Fatal("failed fence rolled back revocation")
	}
}

// Covers: REQ-C2-018
func TestCiIssueClosesAfterLaterPassContract(t *testing.T) {
	t.Parallel()
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint("real=", real), func(t *testing.T) {
			p := people(t)
			f := p.f
			var runStore service.RunStore = f.store.native
			var issues service.IssueStore = f.store.native
			if real {
				runStore = p.state
				issues = p.state
				f.c.Catalog = p.state
				f.c.Events = p.state
			}
			f.c.Runs = runStore
			f.c.People.Issues = issues
			f.restart(t)
			runs := acceptCISequence(t, p)
			finishCISequence(t, p, runs)
			issueID := assertCIRollback(t, p, runStore, runs)
			f.c.Runs = runStore
			f.restart(t)
			if e := f.s.Finished(t.Context(), ciFinish(runs[4], stepmodel.RunPassed)); e != nil {
				t.Fatal(e)
			}
			row, e := issues.ScopedIssue(t.Context(), stateport.IssueScope{Global: true}, issueID)
			if e != nil || row.State != model.IssueClosed || row.CIWorkID != runs[4].ID {
				t.Fatal("later same-branch pass did not close", row, e)
			}
		})
	}
}

type badFinishPublication struct{ service.RunStore }

func (p badFinishPublication) Finish(ctx context.Context, a stateport.Action, u stateport.RunUpdate) error {
	u.Publication.Publication.Repo = "absent/team/repo"
	return p.RunStore.Finish(ctx, a, u)
}

func ciFinish(r stateport.Run, status stepmodel.RunStatus) service.FinishedRun {
	out := service.FinishedRun{RunID: r.ID, Status: status}

	return out
}

func assertEnrollmentIdentity(t *testing.T, result model.InviteCertificate) *x509.Certificate {
	t.Helper()
	cert, e := clientcert.ParseCertificate([]byte(result.Certificate))
	if e != nil {
		t.Fatal(e)
	}
	if result.Identity != "approved" || len(result.Scope) != 1 || result.Scope[0] != "origin" || cert.Subject.CommonName != "approved" || len(cert.DNSNames) != 0 {
		t.Fatal("unapproved CSR identity or scope escaped", result, cert)
	}
	return cert
}

func assertRenewalIdentity(t *testing.T, p *peopleFixture, actor actor.Actor, csr string) {
	t.Helper()
	ctx := t.Context()
	call2, e := p.f.s.PrepareClientRenew(ctx, apicontract.Caller{Actor: actor}, apicontract.ClientRenewArgs{CSR: csr})
	renewed, e := execute(ctx, call2, e)
	if e != nil || renewed.Identity != actor.Identity || renewed.Admin || len(renewed.Scope) != 1 || renewed.Scope[0] != "origin" {
		t.Fatal("renewal changed approved identity", renewed, e)
	}
	if len(p.issuer.Identities) != 2 || p.issuer.Identities[0] != "approved" || p.issuer.Identities[1] != "approved" {
		t.Fatal("issuer trusted CSR identity", p.issuer.Identities)
	}
}

func assertBackupSlot(t *testing.T, p *peopleFixture) {
	t.Helper()
	ctx := t.Context()
	var e error
	p.admin.Fail = nil
	closed := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	done := make(chan error, 1)
	writer := closeWriter{CloseFunc: func() error { close(closed); <-release; return nil }}
	go func() { _, err := p.f.s.Backup(ctx, admin(), writer); done <- err }()
	receive(t, closed)
	if _, e = p.f.s.Backup(ctx, admin(), closeWriter{}); e == nil || !strings.Contains(e.Error(), "backup already running") {
		t.Fatal("backup slot released before stream close", e)
	}
	unblock()
	if e = receive(t, done); e != nil {
		t.Fatal(e)
	}
}

func acceptCISequence(t *testing.T, p *peopleFixture) []stateport.Run {
	t.Helper()
	f := p.f
	runs := []stateport.Run{}
	for n := 1; n <= 5; n++ {
		b := batch(t)
		b.ReceiveID = fmt.Sprint("ci-", n)
		b.Runs[0].Request.ReceiveID = b.ReceiveID
		b.Runs[0].Request.ObjectSHA = fmt.Sprintf("%040x", n)
		b.Runs[0].Request.CommitSHA = new(b.Runs[0].Request.ObjectSHA)
		if n == 4 {
			b.Runs[0].Request.Ref = "refs/heads/other"
		}
		b.Runs[0].Prepared = landedAdmission(t, fmt.Sprintf("%032x", n), b.Runs[0].Request)
		got, e := f.s.Accept(t.Context(), b)
		if e != nil {
			t.Fatal(e)
		}
		runs = append(runs, got.Runs[0])
	}
	return runs
}

func finishCISequence(t *testing.T, p *peopleFixture, runs []stateport.Run) {
	t.Helper()
	f := p.f

	for _, n := range []int{0, 2, 1, 3} {
		status := stepmodel.RunPassed
		if n == 0 || n == 2 {
			status = stepmodel.RunFailed
		}
		if e := f.s.Finished(t.Context(), ciFinish(runs[n], status)); e != nil {
			t.Fatal(e)
		}
		stored, e := f.c.Runs.Run(t.Context(), runs[n].ID)
		if e != nil || stored.Status != status {
			t.Fatal("CI no-op discarded terminal verdict", stored.Status, e)
		}
	}
}

func assertCIRollback(t *testing.T, p *peopleFixture, runStore service.RunStore, runs []stateport.Run) int64 {
	t.Helper()
	f := p.f
	rows, e := p.f.c.People.Issues.ScopedIssues(t.Context(), stateport.IssueScope{Global: true}, model.IssueFilter{Limit: 50})
	if e != nil || len(rows) != 1 || rows[0].State != model.IssueOpen || rows[0].CIWorkID != runs[2].ID || rows[0].Occurrences != 2 {
		t.Fatal("old/other-branch pass closed red issue", rows, e)
	}
	// A publication failure after the projection must roll the verdict and issue back.
	lockCall, e := f.s.PrepareIssueLock(t.Context(), workerCaller(), apicontract.IssueLockArgs{ID: rows[0].ID})
	if _, e = execute(t.Context(), lockCall, e); e != nil {
		t.Fatal(e)
	}
	f.c.Runs = badFinishPublication{RunStore: runStore}
	f.restart(t)
	if e := f.s.Finished(t.Context(), ciFinish(runs[4], stepmodel.RunPassed)); e == nil {
		t.Fatal("bad publication accepted")
	}
	row, e := p.f.c.People.Issues.ScopedIssue(t.Context(), stateport.IssueScope{Global: true}, rows[0].ID)
	if e != nil || row.State != model.IssueOpen || row.CIWorkID != runs[2].ID {
		t.Fatal("rollback retained issue change", row, e)
	}
	assertCIRunUnchanged(t, runStore, runs[4])
	return rows[0].ID
}

func assertCIRunUnchanged(t *testing.T, runStore service.RunStore, r stateport.Run) {
	t.Helper()
	stored, e := runStore.Run(t.Context(), r.ID)
	if e != nil || stored.Status != r.Status || stored.Revision != r.Revision {
		t.Fatal("rollback retained verdict", stored, e)
	}
}
