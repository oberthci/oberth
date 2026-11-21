package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestAuthorityLoadSelectsOnlyCurrentScopedGrants(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*stateport.GrantChange)
		want   bool
	}{
		{"exact", func(*stateport.GrantChange) {}, true},
		{"repository-wide", func(g *stateport.GrantChange) { g.File = "" }, true},
		{"revoked", func(g *stateport.GrantChange) { g.Revoke = true }, false},
		{"dismissed", func(g *stateport.GrantChange) { g.Dismiss = true }, false},
		{"another-repository", func(g *stateport.GrantChange) { g.Repository = "origin/team/other" }, false},
		{"another-file", func(g *stateport.GrantChange) { g.File = ".oberth/release.yaml" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grant := stateport.GrantChange{Kind: stateport.GrantConfigMap, Repository: request().Repository, File: ".oberth/build.yaml", Step: "build", Path: "settings"}
			tc.change(&grant)
			authority := ruleAuthority(t, grant, func(context.Context, string) error {
				t.Error("inspection declared an account")
				return nil
			})
			q, _, err := authority.Load(t.Context(), app.Scope{Mode: admissioncontract.ModeInspect, Preparation: nativebinding.Preparation{Repository: request().Repository, File: ".oberth/build.yaml"}})
			if err != nil {
				t.Fatal(err)
			}
			var want []admissioncontract.GrantEvidence
			if tc.want {
				want = []admissioncontract.GrantEvidence{{Repository: grant.Repository, File: grant.File, Step: grant.Step, Path: grant.Path, Kind: "configmap", Revision: "17", State: admissioncontract.StateVerified}}
			}
			if !reflect.DeepEqual(q.Grants, want) {
				t.Fatalf("authority = %#v; want %#v", q.Grants, want)
			}
		})
	}
}

func TestAuthorityLoadDeclaresAccountsOnlyForSubmission(t *testing.T) {
	t.Parallel()
	grant := stateport.GrantChange{Kind: stateport.GrantStorePath, Repository: request().Repository, File: ".oberth/build.yaml", Step: "build", Path: "ci/token"}
	for _, mode := range []admissioncontract.Mode{admissioncontract.ModeInspect, admissioncontract.ModeSubmit} {
		t.Run(string(mode), func(t *testing.T) {
			var declared []string
			authority := ruleAuthority(t, grant, func(_ context.Context, name string) error {
				declared = append(declared, name)
				return nil
			})
			q, _, err := authority.Load(t.Context(), app.Scope{Mode: mode, Preparation: nativebinding.Preparation{Repository: grant.Repository, File: grant.File}})
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			if mode == admissioncontract.ModeSubmit {
				step, err := identityname.Name(q.Grants[0].Key(), identityname.StepCI)
				if err != nil {
					t.Fatal(err)
				}
				want = []string{q.IdentityRequest.PipelineAccount, step}
			}
			if !reflect.DeepEqual(declared, want) {
				t.Fatalf("declared = %v; want %v", declared, want)
			}
		})
	}
}

func ruleAuthority(t *testing.T, grant stateport.GrantChange, declare func(context.Context, string) error) *app.AuthorityReader {
	t.Helper()
	authority, err := app.NewAuthority(app.AuthorityConfig{
		Namespace: "runs", ServerNamespace: "oberth", ArgoNamespace: "argo", EnsureAccount: declare,
		Grants: func(_ context.Context, repositories []string) (stateport.AuthoritySnapshot, error) {
			if !reflect.DeepEqual(repositories, []string{request().Repository}) {
				t.Errorf("grant lookup scope = %v", repositories)
			}
			return stateport.AuthoritySnapshot{Revision: 17, Grants: []stateport.GrantChange{grant}}, nil
		},
		Templates: func(context.Context, string) (*wf.WorkflowTemplate, error) {
			return nil, errors.New("unexpected template read")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}
