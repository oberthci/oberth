package service_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
)

type accountBackupBuffer struct{ bytes.Buffer }

func (*accountBackupBuffer) Close() error { return nil }

func TestBackupAccountGrantsOnlyItsNamedSnapshotOperation(t *testing.T) {
	p := people(t)
	const account = "system:serviceaccount:oberth:oberth-backup"
	p.f.c.People.BackupAccount = account
	snapshots := 0
	p.f.c.People.Backup = snapshotFunc(func(ctx context.Context, options stateport.Options, out io.Writer) (stateport.BackupReport, error) {
		snapshots++
		return p.state.Snapshot(ctx, options, out)
	})
	p.f.restart(t)
	valid := actor.Actor{Identity: account, Fingerprint: "account-uid", AuthMethod: "service-account"}
	out := &accountBackupBuffer{}
	report, err := p.f.s.BackupAccount(t.Context(), valid, out)
	if err != nil || snapshots != 1 || out.Len() == 0 || !report.Integrity {
		t.Fatal("backup account could not read its canonical snapshot", err)
	}
	if _, err = p.f.s.PrepareAdminVersion(t.Context(), apicontract.Caller{Actor: valid}, apicontract.AdminVersionArgs{}); err == nil {
		t.Fatal("backup account received administrator access")
	}
	for _, wrong := range []actor.Actor{
		{Identity: "system:serviceaccount:oberth-runs:oberth-backup", AuthMethod: "service-account"},
		{Identity: "system:serviceaccount:oberth:other", AuthMethod: "service-account"},
		{Identity: account, AuthMethod: "cert"},
		{Identity: account, AuthMethod: "service-account", Admin: true},
	} {
		if _, err = p.f.s.BackupAccount(t.Context(), wrong, &accountBackupBuffer{}); err == nil {
			t.Fatal("another identity received the backup capability")
		}
	}
	if snapshots != 1 {
		t.Fatal("unauthorized backup reached the snapshotter")
	}
}
