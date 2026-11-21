package app_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"
	argofake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestScheduleEnsuresAccountsWithoutAnyPush(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	accounts := map[string]string{}
	authority, err := app.NewAuthority(app.AuthorityConfig{Namespace: "runs", ServerNamespace: "oberth", ArgoNamespace: "argo", Grants: f.runs.Grants,
		Templates: func(context.Context, string) (*wf.WorkflowTemplate, error) {
			t.Fatal("unexpected template read")
			return nil, nil
		},
		EnsureAccount: func(_ context.Context, name string) error { accounts[name] = "uid-" + name; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.authority.LoadFunc = authority.Load
	executor := argojob.New(argojob.Config{Accounts: argofake.Accounts{Names: accounts}, SecretStore: scheduleStore(t)})
	f.native.HookFunc = executor.Hooks
	s := f.new(t)
	r := request()
	if _, err = s.Check(t.Context(), r); err != nil || len(accounts) != 0 {
		t.Fatal("check created accounts", err, accounts)
	}
	r.ScheduleEntry = "nightly"
	p, err := s.PrepareSchedule(t.Context(), r)
	if err != nil {
		t.Fatal("schedule before first push", err)
	}
	pipeline := p.View().Workflow.Spec.ServiceAccountName
	if accounts[pipeline] == "" || len(accounts) != 1 {
		t.Fatal("pipeline account missing", accounts)
	}

	assertScheduledSecretAccount(t, f, accounts)
}

func assertScheduledSecretAccount(t *testing.T, f *fixture, accounts map[string]string) {
	t.Helper()
	r := request()
	r.ScheduleEntry = "nightly"
	grant := stateport.GrantChange{Kind: stateport.GrantStorePath, Repository: r.Repository, File: ".oberth/build.yaml", Step: "build", Path: "ci/token"}
	if _, err := f.runs.ChangeGrant(t.Context(), stateport.Action{Actor: r.Actor.Identity}, grant); err != nil {
		t.Fatal(err)
	}
	f.source.Read = func(_ context.Context, _ gitport.Revision, file string) (gitport.File, error) {
		w := workflow()
		w.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "ci/token"}
		w.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec", "--", "true"}
		return gitport.File{Path: file, Blob: strings.Repeat("c", 40), Mode: 0100644, Bytes: document(w)}, nil
	}
	f.d.Preparation.HelperImage = "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:" + strings.Repeat("b", 64)
	s := f.new(t)
	p, err := s.PrepareSchedule(t.Context(), r)
	if err != nil {
		t.Fatal("schedule after grant", err)
	}
	step, err := identityname.Name(identityname.Key{Repository: r.Repository, File: grant.File, Step: grant.Step}, identityname.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	w := p.View().Workflow
	if w.Spec.Templates[0].ServiceAccountName != step || accounts[step] == "" || len(accounts) != 2 {
		t.Fatal("scheduled step account missing", accounts)
	}
	rows, err := f.runs.Runs(t.Context(), stateport.RunFilter{Limit: 10})
	if err != nil || len(rows) != 0 || f.creates.Load() != 0 {
		t.Fatal("preparing schedule started a run", err)
	}
}

func scheduleStore(t *testing.T) runtimeconfig.SecretDelivery {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: time.Unix(1700000000, 0), NotAfter: time.Unix(1900000000, 0)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return runtimeconfig.SecretDelivery{Address: "https://store.oberth.svc", ServerName: "store.oberth.svc", CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), Access: runtimeconfig.SecretStoreAccess{Mount: "oberth", AuthMount: "kubernetes", WriterRole: "writer", StepRole: "step", ServerNamespace: "oberth", PipelineNamespace: "runs", ServerAccount: "oberth", Audience: "store"}}
}
