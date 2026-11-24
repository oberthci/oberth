package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
)

func TestAdministrativeDetailAndIdentityBoundaries(t *testing.T) {
	t.Parallel()
	p := people(t)
	ctx := t.Context()
	for _, extra := range []int{0, 1} {
		q := apicontract.AdminAuditRecordArgs{Action: "upgrade.completed", Details: map[string]string{"to": strings.Repeat("x", 2048-len(`{"to":""}`)+extra)}}
		call, e := p.f.s.PrepareAdminAuditRecord(ctx, adminCaller(), q)
		_, e = execute(ctx, call, e)
		if (e != nil) != (extra > 0) {
			t.Fatal("audit detail N/N+1", extra, e)
		}
	}
	for _, q := range []apicontract.AdminAuditRecordArgs{{Action: "arbitrary"}, {Action: "restore.completed"}, {Action: "upgrade.completed", Details: map[string]string{"private_key": "secret"}}, {Action: "upgrade.completed", Details: map[string]string{"rollback": "true"}}, {Action: "rollback.completed", Details: map[string]string{"rollback": "true"}}} {
		call, e := p.f.s.PrepareAdminAuditRecord(ctx, adminCaller(), q)
		if _, e = execute(ctx, call, e); e == nil {
			t.Fatal("unsafe lifecycle receipt accepted")
		}
	}
	for _, extra := range []int{0, 1} {
		_, key := publicKeys(t)
		q := apicontract.UplinkAddArgs{Scope: []string{"*"}, Identity: strings.Repeat("i", 256+extra), SshPublicKey: key}
		call, e := p.f.s.PrepareUplinkAdd(ctx, adminCaller(), q)
		_, e = execute(ctx, call, e)
		if (e != nil) != (extra > 0) {
			t.Fatal("identity N/N+1", extra, e)
		}
	}
}
func TestBackupRealPortAndCloseFailure(t *testing.T) {
	t.Parallel()
	p := people(t)
	var output bytes.Buffer
	writer := closeWriter{WriteFunc: output.Write}
	report, e := p.f.s.Backup(t.Context(), admin(), writer)
	if e != nil {
		t.Fatal(e)
	}
	if e = report.Validate(); e != nil {
		t.Fatal(e)
	}
	inspector, e := store.NewBackup(filepath.Join(t.TempDir(), "inspect.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	inspected, e := inspector.Inspect(t.Context(), p.f.c.People.BackupOptions, bytes.NewReader(output.Bytes()))
	if e != nil || inspected.AuditHead != report.AuditHead {
		t.Fatal("service did not stream current store archive", inspected, e)
	}
	beforeCompleted, e := p.state.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "backup.completed", Limit: 200})
	if e != nil {
		t.Fatal(e)
	}
	failure := errors.New("sink close failed")
	p.f.c.People.Backup = snapshotFunc(func(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error) {
		return stateport.BackupReport{}, nil
	})
	p.f.restart(t)
	if _, e = p.f.s.Backup(t.Context(), admin(), closeWriter{CloseFunc: func() error { return failure }}); !errors.Is(e, failure) {
		t.Fatal("close failure reported success", e)
	}
	afterCompleted, e := p.state.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "backup.completed", Limit: 200})
	if e != nil || len(afterCompleted) != len(beforeCompleted) {
		t.Fatal("backup completed before sink close", afterCompleted, e)
	}
}
func TestIneligibleNativeOutcomesLeaveCIIssueUntouched(t *testing.T) {
	t.Parallel()
	p := people(t)
	f := p.f
	f.c.Runs = p.state
	f.c.Catalog = p.state
	f.c.Events = p.state
	f.restart(t)
	runs := acceptCISequence(t, p)
	if e := f.s.Finished(t.Context(), service.FinishedRun{RunID: runs[0].ID, Status: stepmodel.RunFailed}); e != nil {
		t.Fatal(e)
	}
	before, e := p.state.Issues(t.Context(), nil, model.IssueFilter{Limit: 50})
	if e != nil {
		t.Fatal(e)
	}
	outcomes := []service.FinishedRun{{RunID: runs[1].ID, Status: stepmodel.RunCancelled}, {RunID: runs[2].ID, Status: stepmodel.RunFailed, Reason: stateport.ReasonRestart}, {RunID: runs[3].ID, Status: stepmodel.RunFailed, Failure: &stepmodel.FailureRecord{Class: stepmodel.FailureInfrastructure, Retry: stepmodel.RetryHint{Action: stepmodel.RetryNone}}}}
	for _, o := range outcomes {
		if e = f.s.Finished(t.Context(), o); e != nil {
			t.Fatal(e)
		}
	}
	after, e := p.state.Issues(t.Context(), nil, model.IssueFilter{Limit: 50})
	if e != nil || fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatal("ineligible native verdict changed CI issue", before, after, e)
	}
	call, e := f.s.PrepareIssueClose(t.Context(), workerCaller(), apicontract.IssueCloseArgs{ID: before[0].ID})
	if _, e = execute(t.Context(), call, e); e == nil {
		t.Fatal("CI issue closed without later pass")
	}
}
func TestSSHAndInvitationLifetimePublicBoundaries(t *testing.T) {
	t.Parallel()
	p := people(t)
	for _, extra := range []int{0, 1} {
		_, key := publicKeys(t)
		base := strings.TrimSpace(key) + " "
		key = base + strings.Repeat("c", (64<<10)-len(base)+extra)
		call, e := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), apicontract.UplinkAddArgs{Scope: []string{"*"}, Identity: "bounded-key", SshPublicKey: key})
		_, e = execute(t.Context(), call, e)
		if (e != nil) != (extra > 0) {
			t.Fatal("SSH N/N+1", extra, e)
		}
	}
	for _, extra := range []int{0, 1} {
		q := apicontract.UplinkInviteArgs{Scope: []string{"*"}, Identity: "invited", ServerURL: "https://ci.example/mcp", SecretDigest: digestText("bounded invitation" + fmt.Sprint(extra)), Expires: fmt.Sprint(168+extra) + "h"}
		call, e := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), q)
		_, e = execute(t.Context(), call, e)
		if (e != nil) != (extra > 0) {
			t.Fatal("invitation lifetime N/N+1", extra, e)
		}
	}
}
func TestAssistedFailureDoesNotChangeCIIssue(t *testing.T) {
	t.Parallel()
	p := people(t)
	f := p.f
	f.c.Runs = p.state
	f.c.Catalog = p.state
	f.c.Events = p.state
	f.restart(t)
	runs := acceptCISequence(t, p)
	if e := f.s.Finished(t.Context(), service.FinishedRun{RunID: runs[0].ID, Status: stepmodel.RunFailed}); e != nil {
		t.Fatal(e)
	}
	b := batch(t)
	b.ReceiveID = "assisted-ci"
	b.Runs[0].Request.ReceiveID = b.ReceiveID
	b.Runs[0].Request.Assist = true
	b.Runs[0].Prepared = landedAdmission(t, "abcdef000000000000000000000000ff", b.Runs[0].Request)
	accepted, e := f.s.Accept(t.Context(), b)
	if e != nil {
		t.Fatal(e)
	}
	r := accepted.Runs[0]
	if e = p.state.Assist(t.Context(), stateport.Action{Actor: caller().Identity}, stateport.HoldAction{RunID: r.ID, Step: "build", NodeID: "node", PodUID: "pod", Kind: "exec", Revision: r.Revision, Argv: []string{"true"}, TimeoutS: 60}); e != nil {
		t.Fatal(e)
	}
	if e = f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunFailed}); e != nil {
		t.Fatal("assisted finish refused", e)
	}
	rows, e := p.state.Issues(t.Context(), nil, model.IssueFilter{Limit: 50})
	if e != nil || len(rows) != 1 || rows[0].Occurrences != 1 || rows[0].CIWorkID != runs[0].ID {
		t.Fatal("assisted failure changed CI", rows, e)
	}
}
func TestInvitationSecretPublicBoundaries(t *testing.T) {
	t.Parallel()
	p := people(t)
	for _, n := range []int{31, 32, 512, 513} {
		secret := strings.Repeat("s", n)
		csr, key := publicKeys(t)
		q := apicontract.UplinkInviteArgs{Scope: []string{"*"}, Identity: "bounded-secret", ServerURL: "https://ci.example/mcp", SecretDigest: digestText(secret)}
		call, e := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), q)
		v, e := execute(t.Context(), call, e)
		if e != nil {
			t.Fatal(e)
		}
		_, e = p.f.s.Redeem(t.Context(), v.ID, model.InviteRedemption{Secret: secret, ServerURL: v.ServerURL, CAPin: v.CAPin, CSR: csr, SSHPublicKey: key})
		want := n < 32 || n > 512
		if (e != nil) != want {
			t.Fatal("secret bounds", n, e)
		}
	}
}
func TestIssueLockExpiryBoundary(t *testing.T) {
	t.Parallel()
	p := people(t)
	v := createTask(t, p, "origin", "lock boundary")
	ctx := t.Context()
	call, e := p.f.s.PrepareIssueLock(ctx, workerCaller(), apicontract.IssueLockArgs{ID: v.ID})
	if _, e = execute(ctx, call, e); e != nil {
		t.Fatal(e)
	}
	for _, offset := range []time.Duration{5*time.Minute - time.Nanosecond, 5 * time.Minute} {
		p.f.offset.Store(int64(offset))
		call, e := p.f.s.PrepareIssueUpdate(ctx, adminCaller(), apicontract.IssueUpdateArgs{ID: v.ID, Title: new("changed")})
		_, e = execute(ctx, call, e)
		if (e != nil) != (offset < 5*time.Minute) {
			t.Fatal("five minute lock boundary", offset, e)
		}
	}
}
func TestBearerMinimumLifetimeBoundary(t *testing.T) {
	t.Parallel()
	p := people(t)
	for _, d := range []time.Duration{time.Hour - time.Second, time.Hour} {
		_, key := publicKeys(t)
		q := apicontract.UplinkAddArgs{Scope: []string{"*"}, Identity: "minimum", SshPublicKey: key, BearerDigest: apicontract.SecretDigest("test-only-bearer"), TTL: d.String()}
		call, e := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), q)
		_, e = execute(t.Context(), call, e)
		if (e != nil) != (d < time.Hour) {
			t.Fatal("minimum lifetime boundary", d, e)
		}
	}
}
func TestIssueMutationBodyBoundaries(t *testing.T) {
	t.Parallel()
	p := people(t)
	v := createTask(t, p, "origin", "")
	ctx := t.Context()
	projected := v.Clone()
	projected.Revision++
	projected.UpdatedAt = p.f.c.Now().UTC()
	raw, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	capacity := model.IssueBytes - len(raw)
	for _, extra := range []int{0, 1} {
		body := strings.Repeat("b", capacity+extra)
		call, e := p.f.s.PrepareIssueUpdate(ctx, workerCaller(), apicontract.IssueUpdateArgs{ID: v.ID, Body: &body})
		_, e = execute(ctx, call, e)
		if (e != nil) != (extra > 0) {
			t.Fatal("update complete issue N/N+1", extra, e)
		}
	}
	body := strings.Repeat("b", capacity-1)
	update, e := p.f.s.PrepareIssueUpdate(ctx, workerCaller(), apicontract.IssueUpdateArgs{ID: v.ID, Body: &body})
	if _, e = execute(ctx, update, e); e != nil {
		t.Fatal(e)
	}
	for _, extra := range []int{0, 1} {
		call, e := p.f.s.PrepareIssueAppend(ctx, workerCaller(), apicontract.IssueAppendArgs{ID: v.ID, Text: "b"})
		_, e = execute(ctx, call, e)
		if (e != nil) != (extra > 0) {
			t.Fatal("append complete issue N/N+1", extra, e)
		}
	}
}
