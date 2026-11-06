package app

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func newDirectFragmentFixture(t *testing.T) *directPlacementFixture {
	return newDirectFragmentFixtureExpiring(t, 0)
}
func newDirectFragmentFixtureExpiring(t *testing.T, lifetime time.Duration) *directPlacementFixture {
	t.Helper()
	source := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  templates:
  - name: ci
    dag:
      tasks:
      - name: producer
        templateRef: {name: github/org/shared@v1, template: build}
`)
	fragment := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  templates:
  - name: build
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
      resources:
        requests: {cpu: 100m, memory: 64Mi}
        limits: {cpu: 1, memory: 128Mi}
`)
	key, err := argoworkflow.ParseFragmentRef("github/org/shared@v1")
	if err != nil {
		t.Fatal(err)
	}
	return newDirectPlacementFixtureInputs(t, lifetime, source, map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: {Key: key, SHA: strings.Repeat("e", 40), Source: fragment}})
}

func TestDirectProtectedFragmentAdmissionAndParent(t *testing.T) {
	f := newDirectFragmentFixture(t)
	if err := f.executor.Execute(t.Context(), f.request, f.requirement, io.Discard); err != nil {
		t.Fatal(err)
	}
	a, err := f.database.PlacementAdmission(t.Context(), f.request.Run.ID)
	if err != nil || a == nil || a.Version != 2 || a.Sources == nil || len(a.Sources.Fragments) != 1 {
		t.Fatal("missing original graph custody", err)
	}
	if _, err = f.database.SetRunWorkflowName(t.Context(), f.request.Run.ID, f.request.WorkflowName); err != nil {
		t.Fatal(err)
	}
	if err = f.jobs.CreateWorkflow(t.Context(), f.request); err != nil {
		t.Fatal(err)
	}
	if f.workflows.posts != 2 {
		t.Fatal("distinct phases did not preserve single parent")
	}
	if _, err = f.database.VerifyAuditState(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestDirectProtectedFragmentRestartIgnoresChangedResolverForCleanup(t *testing.T) {
	for _, mode := range []string{"absent resolver", "changed resolver", "revoked grant", "removed config and recipes"} {
		t.Run(mode, func(t *testing.T) {
			f := newDirectFragmentFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			f.workflows.lostReply = true
			if f.executor.Execute(ctx, f.request, f.requirement, io.Discard) == nil {
				t.Fatal("lost reply accepted")
			}
			execution, err := f.database.KVMPreflight(ctx, f.request.Run.ID)
			if err != nil || execution.Cleaned || !execution.Submitted {
				t.Fatal("uncertain obligation lost", err)
			}
			reads := len(f.fragmentBlobs.readArgs)
			if err = f.database.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := storetest.Open(ctx, f.databasePath, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			config := f.jobs.config
			loader := f.jobs.fragments
			switch mode {
			case "absent resolver":
				loader = nil
			case "changed resolver":
				f.fragmentBlobs.reachErr = context.DeadlineExceeded
			case "revoked grant":
				if _, err = db.SetWorkflowCapabilityGrant(ctx, f.request.Repository.ID, "operator", nil); err != nil {
					t.Fatal(err)
				}
			case "removed config and recipes":
				config.ExecutionPlacement = nil
				config.TrustedKVMGroup = 0
				loader = nil
			}
			f.workflows.readsUnavailable = false
			native, err := argojob.NewController(f.workflows, f.environment.kube, config)
			if err != nil {
				t.Fatal(err)
			}
			native.WithProtectedPlacementJournal(db, f.environment.definition.Transport).WithWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
			jobs, err := NewArgoJobs(native, config, db, emptySecretAccess{}, loader)
			if err != nil {
				t.Fatal(err)
			}
			jobs.SetWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
			jobs.SetResourceBlobs(f.jobs.resourceBlobs)
			var executor *TrustedPreflights
			if mode == "removed config and recipes" {
				executor, err = NewProtectedPreflightRecovery(jobs, db, f.executor.workspaceRoot, func(context.Context) error { return nil })
			} else {
				executor, err = NewGenericProtectedPreflights(jobs, db, f.executor.recipes, f.executor.workspaceRoot, func(context.Context) error { return nil })
			}
			if err != nil {
				t.Fatal(err)
			}
			// Current authority must fail before cleanup; retained bytes never bypass it.
			if executor.CheckProtectedPlacementAuthority(ctx, f.request.Run.ID) == nil {
				t.Fatal("changed current authority accepted")
			}
			policyReads := f.jobs.resourceBlobs.(*directPolicyFixture).fragmentPolicyReads
			if _, err = executor.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			execution, err = db.KVMPreflight(ctx, f.request.Run.ID)
			if err != nil || !execution.Cleaned || execution.Receipt != nil || f.workflows.posts != 1 || f.workflows.deletes != 1 {
				t.Fatal("original fragment workflow stranded or recreated", err)
			}
			if f.jobs.resourceBlobs.(*directPolicyFixture).fragmentPolicyReads != policyReads {
				t.Fatal("cleanup refreshed current producer policy")
			}
			if len(f.fragmentBlobs.readArgs) != reads {
				t.Fatal("cleanup reloaded current fragment source")
			}
			if _, err = db.VerifyAuditState(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDirectProtectedFragmentChangedPolicyRefusesReceipt(t *testing.T) {
	f := newDirectFragmentFixture(t)
	f.workflows.afterCreate = func() { f.jobs.fragments = nil }
	if f.executor.Execute(t.Context(), f.request, f.requirement, io.Discard) == nil {
		t.Fatal("removed fragment authority accepted receipt")
	}
	execution, err := f.database.KVMPreflight(t.Context(), f.request.Run.ID)
	if err != nil || execution.Receipt != nil || !execution.Cleaned || f.workflows.posts != 1 || f.workflows.deletes != 1 {
		t.Fatal("authority change lost original cleanup or accepted evidence", err)
	}
}

func TestDirectProtectedFragmentSchedulerPublicationChecksCurrentGraph(t *testing.T) {
	for _, mode := range []string{"unchanged", "missing resolver", "changed commit", "changed fragment bytes", "changed root", "producer refresh failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newDirectFragmentFixture(t)
			ctx := t.Context()
			if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := f.database.SetRunWorkflowName(ctx, f.request.Run.ID, f.request.WorkflowName); err != nil {
				t.Fatal(err)
			}
			if err := f.jobs.CreateWorkflow(ctx, f.request); err != nil {
				t.Fatal(err)
			}
			policy := f.jobs.resourceBlobs.(*directPolicyFixture)
			switch mode {
			case "missing resolver":
				f.jobs.fragments = nil
			case "changed commit":
				for key := range f.fragmentBlobs.tags {
					f.fragmentBlobs.tags[key] = strings.Repeat("f", 40)
				}
			case "changed fragment bytes":
				for sha, body := range f.fragmentBlobs.blobs {
					f.fragmentBlobs.blobs[sha] = "# changed\n" + body
				}
			case "changed root":
				policy.source = append([]byte("# changed\n"), policy.source...)
			case "producer refresh failure":
				policy.fragmentPolicyErr = context.DeadlineExceeded
			}
			logs, err := runlog.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := service.NewScheduler(service.SchedulerConfig{Store: f.database, Git: policy, Logs: logs, Jobs: f.jobs, ReleaseJobs: f.jobs, TrustedPreflight: f.executor, Auditor: f.database, WorkspaceRoot: t.TempDir(), MutationGate: func(context.Context) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			pub, err := f.database.BeginPublication(ctx, model.PublicationSpec{RepoID: f.request.Repository.ID, RunID: f.request.Run.ID, RefKind: model.RefBranch, Ref: f.request.Run.Ref, ResultSHA: f.request.Run.SHA, Actor: f.request.Run.Actor})
			if err != nil {
				t.Fatal(err)
			}
			beforeRefresh := policy.fragmentPolicyReads
			result, err := scheduler.DeliverPublication(ctx, pub.ID)
			if policy.fragmentPolicyReads <= beforeRefresh {
				t.Fatal("publication reused cached producer policy")
			}
			if mode == "unchanged" {
				if err != nil || result.Publication.Status != model.PublicationDelivered || policy.pushes != 1 {
					t.Fatal("unchanged fragment publication refused", err)
				}
			} else if policy.pushes != 0 || result.Publication.Status == model.PublicationDelivered {
				t.Fatal("changed fragment policy published", err)
			}
		})
	}
}

func TestDirectProtectedFragmentExpiredParentRestartsWithoutResolver(t *testing.T) {
	f := newDirectFragmentFixtureExpiring(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.SetRunWorkflowName(ctx, f.request.Run.ID, f.request.WorkflowName); err != nil {
		t.Fatal(err)
	}
	f.workflows.lostReply = true
	if f.jobs.CreateWorkflow(ctx, f.request) == nil {
		t.Fatal("lost parent reply accepted")
	}
	raw, err := json.Marshal(f.jobs.config.ExecutionPlacement)
	if err != nil {
		t.Fatal(err)
	}
	var scope struct {
		Expires time.Time `json:"expires"`
	}
	if err = json.Unmarshal(raw, &scope); err != nil {
		t.Fatal(err)
	}
	if err = f.database.Close(); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Until(scope.Expires))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	db, err := storetest.Open(ctx, f.databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	config := f.jobs.config
	config.ExecutionPlacement, err = argojob.ParseExecutionPlacementForStartup(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.workflows.readsUnavailable = false
	native, err := argojob.NewController(f.workflows, f.environment.kube, config)
	if err != nil {
		t.Fatal(err)
	}
	native.WithProtectedPlacementJournal(db, f.environment.definition.Transport).WithWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
	jobs, err := NewArgoJobs(native, config, db, emptySecretAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs.SetWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
	jobs.SetResourceBlobs(f.jobs.resourceBlobs)
	executor, err := NewGenericProtectedPreflights(jobs, db, f.executor.recipes, f.executor.workspaceRoot, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if executor.CheckProtectedPlacementAuthority(ctx, f.request.Run.ID) == nil || jobs.CreateWorkflow(ctx, f.request) == nil {
		t.Fatal("expired retained graph authorized execution")
	}
	clean := false
	for attempt := 0; attempt < 5; attempt++ {
		clean, err = native.SettleCreate(ctx, f.request.WorkflowName, f.request.Run.ID, time.Now().Add(-time.Second))
		if clean || err != nil {
			break
		}
	}
	if err != nil || !clean || f.workflows.posts != 2 || f.workflows.deletes != 2 {
		t.Fatal("expired parent graph cleanup stranded", err)
	}
	if _, err = db.VerifyAuditState(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDirectProtectedFragmentExpiredPreflightRestartsWithoutResolver(t *testing.T) {
	f := newDirectFragmentFixtureExpiring(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f.workflows.lostReply = true
	if f.executor.Execute(ctx, f.request, f.requirement, io.Discard) == nil {
		t.Fatal("lost preflight reply accepted")
	}
	raw, err := json.Marshal(f.jobs.config.ExecutionPlacement)
	if err != nil {
		t.Fatal(err)
	}
	var scope struct {
		Expires time.Time `json:"expires"`
	}
	if err = json.Unmarshal(raw, &scope); err != nil {
		t.Fatal(err)
	}
	if err = f.database.Close(); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Until(scope.Expires))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	db, err := storetest.Open(ctx, f.databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	config := f.jobs.config
	config.ExecutionPlacement, err = argojob.ParseExecutionPlacementForStartup(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.workflows.readsUnavailable = false
	native, err := argojob.NewController(f.workflows, f.environment.kube, config)
	if err != nil {
		t.Fatal(err)
	}
	native.WithProtectedPlacementJournal(db, f.environment.definition.Transport).WithWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
	jobs, err := NewArgoJobs(native, config, db, emptySecretAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs.SetWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
	jobs.SetResourceBlobs(f.jobs.resourceBlobs)
	executor, err := NewGenericProtectedPreflights(jobs, db, f.executor.recipes, f.executor.workspaceRoot, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if executor.CheckProtectedPlacementAuthority(ctx, f.request.Run.ID) == nil || jobs.CreateWorkflow(ctx, f.request) == nil {
		t.Fatal("expired retained graph authorized execution")
	}
	if _, err = executor.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	execution, err := db.KVMPreflight(ctx, f.request.Run.ID)
	if err != nil || !execution.Cleaned || execution.Receipt != nil || f.workflows.posts != 1 || f.workflows.deletes != 1 {
		t.Fatal("expired preflight graph cleanup stranded", err)
	}
	if _, err = db.VerifyAuditState(ctx); err != nil {
		t.Fatal(err)
	}
}
