package app_test

import (
	"context"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"
	argofake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestReleaseProjectionUsesAdmissionsStepWithoutAccountConfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, step string
		release    bool
	}{
		{"exact-release", "publish", true}, {"wildcard-release", "*", true}, {"ci", "publish", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := realStore(t)
			scope := nativebinding.Preparation{Repository: request().Repository, File: ".oberth/release.yaml", Release: tc.release}
			document := identityDocument("publish")
			if _, err := prepareReleaseAccounts(t, p, scope, document); err == nil {
				t.Fatal("projection bypassed its audience grant")
			}
			grantAudience(t, p, tc.step)
			prepared, err := prepareReleaseAccounts(t, p, scope, document)
			if err != nil {
				t.Fatal(err)
			}
			if tc.release {
				assertReleaseAccount(t, prepared, scope, tc.step)
			} else {
				assertCIAccount(t, prepared, scope)
			}
		})
	}
}

func TestReleaseAudienceGrantKeepsExactRepositoryFileAndStep(t *testing.T) {
	t.Parallel()
	p := realStore(t)
	grantAudience(t, p, "publish")
	for _, tc := range []struct {
		name, repo, file, step string
		want                   bool
	}{
		{"exact", request().Repository, ".oberth/release.yaml", "publish", true},
		{"renamed-file", request().Repository, ".oberth/renamed.yaml", "publish", false},
		{"same-template-other-file", request().Repository, ".oberth/other.yaml", "publish", false},
		{"other-template", request().Repository, ".oberth/release.yaml", "another", false},
		{"other-repository", "origin/team/other", ".oberth/release.yaml", "publish", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := nativebinding.Preparation{Repository: tc.repo, File: tc.file, Release: true}
			prepared, err := prepareReleaseAccounts(t, p, scope, identityDocument(tc.step))
			if (err == nil) != tc.want {
				t.Fatalf("admitted=%v want=%v: %v", err == nil, tc.want, err)
			}
			if tc.want {
				assertReleaseAccount(t, prepared, scope, tc.step)
			}
		})
	}
}

func TestAuthoredFactsCannotChooseReleaseAccounts(t *testing.T) {
	t.Parallel()
	p := realStore(t)
	grantAudience(t, p, "publish")
	document := strings.Replace(identityDocument("publish"), "spec:\n", "spec:\n  serviceAccountName: injected-account\n  arguments:\n    parameters: [{name: release, value: 'true'}, {name: release-kind, value: verified}]\n", 1)
	document = strings.Replace(document, "    container:\n", "    serviceAccountName: injected-step\n    container:\n", 1)
	for _, release := range []bool{false, true} {
		scope := nativebinding.Preparation{Repository: request().Repository, File: ".oberth/release.yaml", Release: release}
		prepared, err := prepareReleaseAccounts(t, p, scope, document)
		if err != nil {
			t.Fatal(err)
		}
		if release {
			assertReleaseAccount(t, prepared, scope, "publish")
		} else {
			assertCIAccount(t, prepared, scope)
		}
	}
}

func grantAudience(t *testing.T, p *store.Store, step string) {
	t.Helper()
	_, err := p.ChangeGrant(t.Context(), stateport.Action{Actor: request().Actor.Identity}, stateport.GrantChange{
		Kind: stateport.GrantAudience, Repository: request().Repository, File: ".oberth/release.yaml", Step: step, Path: "publisher",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertCIAccount(t *testing.T, prepared *admissioncontract.PreparedAdmission, scope nativebinding.Preparation) {
	t.Helper()
	want, err := identityname.Name(identityname.Key{Repository: scope.Repository}, identityname.PipelineCI)
	if err != nil {
		t.Fatal(err)
	}
	w := prepared.View().Workflow
	if w.Spec.ServiceAccountName != want || w.Spec.Executor.ServiceAccountName != want || w.Spec.Templates[0].ServiceAccountName != want {
		t.Fatal("CI acquired a release account")
	}
}

func prepareReleaseAccounts(t *testing.T, p *store.Store, scope nativebinding.Preparation, document string) (*admissioncontract.PreparedAdmission, error) {
	t.Helper()
	accounts := map[string]string{}
	authority, err := app.NewAuthority(app.AuthorityConfig{Namespace: "runs", ServerNamespace: "oberth", ArgoNamespace: "argo", Grants: p.Grants,
		Templates: func(context.Context, string) (*wf.WorkflowTemplate, error) {
			t.Fatal("unexpected template read")
			return nil, nil
		},
		EnsureAccount: func(_ context.Context, name string) error { accounts[name] = "uid-" + name; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	q, policy, err := authority.Load(t.Context(), app.Scope{Mode: admissioncontract.ModeSubmit, Preparation: scope})
	if err != nil {
		return nil, err
	}
	q.Mode, q.Repository, q.SelectedFile, q.RunID = admissioncontract.ModeSubmit, scope.Repository, scope.File, "release-run"
	q.Ref, q.Trigger = "refs/tags/v1.2.3", model.TriggerTag
	q.ObjectSHA, q.CommitSHA, q.FileBlob = strings.Repeat("a", 40), strings.Repeat("a", 40), strings.Repeat("b", 40)
	q.Document = []byte(document)
	q.SourceReceipt = sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: q.Repository, Commit: q.CommitSHA}, ObjectSHA: q.ObjectSHA, File: q.SelectedFile, Blob: q.FileBlob, DocumentDigest: sourcecontract.Digest(q.Document)}
	executor := argojob.New(argojob.Config{Accounts: argofake.Accounts{Names: accounts}})
	return argoworkflow.Prepare(t.Context(), q, policy, executor.Hooks(scope))
}

func assertReleaseAccount(t *testing.T, prepared *admissioncontract.PreparedAdmission, scope nativebinding.Preparation, step string) {
	t.Helper()
	want, err := identityname.Name(identityname.Key{Repository: scope.Repository, File: scope.File, Step: step}, identityname.Step)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := identityname.Name(identityname.Key{Repository: scope.Repository}, identityname.Pipeline)
	if err != nil {
		t.Fatal(err)
	}
	w := prepared.View().Workflow
	if w.Spec.ServiceAccountName != pipeline || w.Spec.Executor.ServiceAccountName != pipeline || w.Spec.Templates[0].ServiceAccountName != want || w.Spec.Templates[0].Executor.ServiceAccountName != pipeline {
		t.Fatal("release identity is not the deterministic step account")
	}
}

func identityDocument(step string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ` + step + `
  templates:
  - name: ` + step + `
    volumes:
    - name: identity
      projected:
        sources:
        - serviceAccountToken: {audience: publisher, expirationSeconds: 600, path: token}
    container:
      volumeMounts: [{name: identity, mountPath: /run/identity, readOnly: true}]
      image: oberth.ci/build:1
      command: ['true']
`
}
