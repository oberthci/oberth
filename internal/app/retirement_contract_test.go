package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type retirementContractGit struct {
	retirementFixture
	sources   []gitcache.RetirementSource
	afterRead func()
}

func (g *retirementContractGit) RetirementSources(context.Context, string) ([]gitcache.RetirementSource, error) {
	if g.afterRead != nil {
		g.afterRead()
	}
	return g.sources, nil
}

// Use the installed executor constructors and real repository/grant catalog.
// This checks the census boundary without executing a guest or creating a run.
func TestRetirementRetainsProtectedContractWithCurrentGenericAuthority(t *testing.T) {
	for _, scenario := range []string{"generic", "no executor", "legacy", "recovery", "zero group", "no recipe", "unapproved pair", "missing grant", "wrong namespace", "wrong device", "revoked during scan", "invalid contract", "old retained ref", "legacy sibling", "pending run"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			db, err := storetest.Open(ctx, filepath.Join(t.TempDir(), "state.sqlite"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			upstream, err := storetest.CreateUpstream(db, ctx, model.UpstreamSpec{Name: "forge", Kind: "ssh", BaseURL: "ssh://example.org/team"})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := db.CreateRepository(ctx, model.RepositorySpec{Name: "repository", UpstreamID: upstream.ID, DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			fixture := newRaceFixture(t)
			controller := &protectedControllerFixture{blockingController: newBlockingController()}
			fixture.jobs.controller = controller
			fixture.jobs.config.TrustedKVMGroup = 1234
			fixture.jobs.config.KVMEnabled = scenario == "legacy"
			fixture.jobs.SetWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
			capability := &argoworkflow.Capability{Namespace: fixture.jobs.config.Namespace, ServiceAccount: "operator-runner", ExtendedResources: map[string]int64{protectedtests.GenericDeviceResource: 1}}
			switch scenario {
			case "missing grant":
				capability = nil
			case "wrong namespace":
				capability.Namespace = "different-namespace"
			case "wrong device":
				capability.ExtendedResources = map[string]int64{"example.org/other": 1}
			case "zero group":
				fixture.jobs.config.TrustedKVMGroup = 0
			}
			if capability != nil {
				if _, err = db.SetWorkflowCapabilityGrant(ctx, repo.ID, "operator", capability); err != nil {
					t.Fatal(err)
				}
			}
			recipe := vmrunner.KVMRecipe{SuiteSHA256: strings.Repeat("a", 64), GuestSHA256: strings.Repeat("b", 64), HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("c", 64), SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"}
			constructor := NewGenericProtectedPreflights
			if scenario == "legacy" {
				constructor = NewTrustedPreflights
			}
			p, err := constructor(fixture.jobs, db, []vmrunner.KVMRecipe{recipe}, t.TempDir(), func(context.Context) error { return errors.New("read-only census must not request mutation") })
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "no executor" {
				p = nil
			}
			if scenario == "recovery" {
				p, err = NewProtectedPreflightRecovery(fixture.jobs, db, t.TempDir(), func(context.Context) error { return errors.New("no mutation") })
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "no recipe" {
				p.recipes = nil
			}
			body := []byte(fmt.Sprintf("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: %s\nguest_sha256: %s\nartifacts: [build/program]\n", recipe.SuiteSHA256, recipe.GuestSHA256))
			original := string(body)
			if scenario == "unapproved pair" {
				body = []byte(strings.ReplaceAll(original, recipe.GuestSHA256, strings.Repeat("d", 64)))
			}
			if scenario == "invalid contract" {
				body = append(body, []byte("command: bypass\n")...)
			}
			git := &retirementContractGit{sources: []gitcache.RetirementSource{{Ref: "refs/heads/main", SHA: strings.Repeat("a", 40), Path: protectedtests.ContractFile, Body: body}}}
			if scenario == "old retained ref" {
				git.sources = append(git.sources, gitcache.RetirementSource{Ref: "refs/heads/retained", SHA: strings.Repeat("b", 40), Path: protectedtests.ContractFile, Body: []byte(strings.ReplaceAll(original, recipe.GuestSHA256, strings.Repeat("d", 64)))})
			}
			if scenario == "legacy sibling" {
				git.sources = append(git.sources, gitcache.RetirementSource{Ref: "refs/heads/main", SHA: strings.Repeat("a", 40), Path: ".oberth/build.yaml", Body: []byte("metadata: {annotations: {oberth.ci/kvm-templates: main}}")})
			}
			if scenario == "revoked during scan" {
				git.afterRead = func() {
					if _, e := db.SetWorkflowCapabilityGrant(ctx, repo.ID, "operator", nil); e != nil {
						t.Fatal(e)
					}
				}
			}
			if scenario == "pending run" {
				sha := strings.Repeat("a", 40)
				if _, err := storetest.EnqueueRun(db, ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: sha, TestedSHA: sha, Actor: "operator", Trigger: "push"}); err != nil {
					t.Fatal(err)
				}
			}
			err = (LegacyRemovalCheck{Catalog: db, Git: git, Protected: p}).Check(ctx)
			if (err == nil) != (scenario == "generic") {
				t.Fatalf("census = %v", err)
			}
			if scenario == "old retained ref" && !strings.Contains(err.Error(), "refs/heads/retained") {
				t.Fatalf("lost retained-ref identity: %v", err)
			}
			if scenario == "revoked during scan" && !strings.Contains(err.Error(), "capability changed") {
				t.Fatalf("lost current authority: %v", err)
			}
			if scenario == "generic" && string(git.sources[0].Body) != original {
				t.Fatal("contract changed")
			}
		})
	}
}
