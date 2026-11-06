package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestProtectedPreflightCannotFallThroughOrdinaryExternalTarget(t *testing.T) {
	for _, mode := range []string{"branch", "legacy branch", "other branch", "release", "schedule", "removed target"} {
		t.Run(mode, func(t *testing.T) {
			f := newTargetRouteFixture(t)
			request := f.request
			if mode == "removed target" {
				if _, err := f.jobs.routes.forRequest(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				f.jobs.routes.definitions = map[string]ExecutionTarget{}
			}
			switch mode {
			case "other branch":
				request.Run.Ref = "other"
			case "release":
				request.Run.RefKind = model.RefTag
				request.Run.Ref = "v1.0.0"
				request.Run.Trigger = "tag"
				request.Run.Release = true
				request.Run.Credentialed = true
			case "schedule":
				request.Run.Trigger = "schedule"
				request.Run.ScheduleEntry = "nightly"
			}
			f.jobs.config.TrustedKVMGroup = 1234
			capability := &argoworkflow.Capability{Namespace: f.jobs.config.Namespace, ServiceAccount: "operator-runner", ExtendedResources: map[string]int64{protectedtests.GenericDeviceResource: 1}}
			if _, err := f.db.SetWorkflowCapabilityGrant(t.Context(), request.Repository.ID, "operator", capability); err != nil {
				t.Fatal(err)
			}
			recipe := vmrunner.KVMRecipe{SuiteSHA256: strings.Repeat("a", 64), GuestSHA256: strings.Repeat("b", 64), HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("c", 64), SuiteURL: "https://releases.example.org/suite", GuestURL: "https://releases.example.org/guest"}
			constructor := NewGenericProtectedPreflights
			if mode == "legacy branch" {
				constructor = NewTrustedPreflights
				f.jobs.config.KVMEnabled = true
			}
			p, err := constructor(f.jobs, f.db, []vmrunner.KVMRecipe{recipe}, t.TempDir(), func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			fetches := 0
			p.fetch = func(context.Context, string, string, int64) ([]byte, error) {
				fetches++
				return nil, errors.New("unexpected fetch")
			}
			contract := vmrunner.Contract{Version: 2, Profile: vmrunner.KVMSuiteAMD64Profile, SuiteSHA256: recipe.SuiteSHA256, GuestSHA256: recipe.GuestSHA256, Artifacts: []string{"program"}}
			err = p.Execute(t.Context(), request, vmrunner.Requirement{Contract: &contract}, io.Discard)
			if !errors.Is(err, ErrProtectedTargetUnqualified) || fetches != 0 {
				t.Fatalf("unqualified route progressed: fetch=%d err=%v", fetches, err)
			}
			if _, err = f.db.KVMPreflight(t.Context(), request.Run.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("route refusal reserved preflight: %v", err)
			}
			f.noLocalEffects(t)
			if f.remote.posts != 0 || f.remote.deletes != 0 || *f.execs != 0 {
				t.Fatal("route refusal reached remote effects")
			}
		})
	}
}

func TestProtectedTargetBoundaryKeepsUnrelatedLocalAdmission(t *testing.T) {
	f := newTargetRouteFixture(t)
	request := f.request
	request.Repository.Name = "another"
	if err := f.jobs.checkProtectedTarget(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	f.noLocalEffects(t)
}
