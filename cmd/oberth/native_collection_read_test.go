package main

import (
	"context"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	"github.com/oberthci/oberth/internal/actor"
	apifake "github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

// Unused native ports panic if a retained read tries to reach execution.
type collectedNativePorts struct {
	service.MovingGit
	service.HoldExec
	service.Git
	service.Source
	service.Starter
}

type retainedReadAuthority struct{}

func (retainedReadAuthority) Current(_ context.Context, a actor.Actor) (actor.Actor, error) {
	return a, nil
}
func (retainedReadAuthority) KeyPolicy(context.Context, actor.Actor) (service.KeyPolicy, error) {
	return service.KeyPolicy{}, nil
}

func TestRunGetAndLogsSurviveNativeCollection(t *testing.T) {
	t.Parallel()
	reader, run := realServerLogReaderFor(t, starterWorkflow(), "18551855185518551855185518551855")
	db := reader.owner.c.store
	b := nativebinding.RunBinding{Workflow: nativebinding.Object{Namespace: "runs", Name: "collected", UID: "native-uid"}, RepositoryID: run.RepositoryID, RunID: run.ID, Revision: sourcecontract.Revision{Repository: run.Repo, Commit: *run.CommitSHA}, Ref: run.Ref, Trigger: string(run.Origin.Trigger), Identity: run.Admission.Digest, SpecSHA256: run.Admission.SpecDigest}
	if err := db.BindWorkflow(t.Context(), stateport.Action{Actor: run.Origin.Actor}, b); err != nil {
		t.Fatal(err)
	}
	saveReaderStep(t, reader, run, stepmodel.RunStep{Path: run.Admission.Plan.Entries[0].Path, Name: "build", Status: stepmodel.StepPassed})
	captureReaderContainers(t, reader, run)
	run, err := db.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: run.ID, Revision: run.Revision, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	pods := kubefake.NewSimpleClientset(&core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: "pod", UID: "pod-uid"}}).CoreV1().Pods("runs")
	if err = pods.Delete(t.Context(), "pod", meta.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = pods.Get(t.Context(), "pod", meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("native Pod still exists", err)
	}
	assertRetainedReads(t, reader, run.ID)
	native := argofake.NewSimpleClientset(&wf.Workflow{ObjectMeta: meta.ObjectMeta{Namespace: b.Workflow.Namespace, Name: b.Workflow.Name, UID: "native-uid"}}).ArgoprojV1alpha1().Workflows(b.Workflow.Namespace)
	if err = native.Delete(t.Context(), b.Workflow.Name, meta.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = native.Get(t.Context(), b.Workflow.Name, meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("native Workflow still exists", err)
	}
	assertRetainedReads(t, reader, run.ID)
}

func assertRetainedReads(t *testing.T, reader serverLogs, id string) {
	t.Helper()
	db, absent := reader.owner.c.store, collectedNativePorts{}
	cfg := service.Config{Publications: db, Promotions: db, MovingGit: absent, HoldStore: db, Holds: absent, Runs: db, Catalog: db, Events: db, Git: absent, Source: absent, Exec: reader.owner, Starter: absent, Logs: reader, Authority: retainedReadAuthority{}, Now: apifake.NewClock().Now, NewID: func() string { return "unused" }, OnError: func(err error) { t.Error(err) }, AfterFunc: func(d time.Duration, f func()) service.Timer { return time.AfterFunc(d, f) }}
	svc, err := service.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	caller := apicontract.Caller{Actor: actor.Actor{Identity: "operator", AuthMethod: "cert", Admin: true}}
	get, err := svc.PrepareRunGet(t.Context(), caller, apicontract.RunGetArgs{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	record, err := get(t.Context())
	if err != nil || record.Status != stepmodel.RunPassed || len(record.Steps) != 1 || record.Steps[0].Status != stepmodel.StepPassed {
		t.Fatal("run get needs the collected native Workflow", record, err)
	}
	read, err := svc.PrepareRunLogs(t.Context(), caller, apicontract.RunLogsArgs{ID: id, Step: "build"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := read(t.Context())
	if err != nil || output.Content != "main output\n" {
		t.Fatal("run logs needs the collected native Workflow", output, err)
	}
}
