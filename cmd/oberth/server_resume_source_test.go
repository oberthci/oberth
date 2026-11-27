package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

func TestResumeSourceOriginalLocatorFollowsDurableSuccessors(t *testing.T) {
	f := newDeliveryFixture(t)
	first, err := f.o.c.store.Run(t.Context(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	second := f.acceptSourceRun(t, first, true)
	caller := sourcedelivery.Caller{Namespace: first.Admission.Identity.Namespace, Workflow: first.Origin.WorkflowName, UID: first.Origin.NativeUID(), Pod: f.pod.DeepCopy()}
	caller.Pod.Status.Phase = core.PodRunning
	f.assertResumeSourceLoad(t, caller, first.ID, *second.CommitSHA)
	third := f.acceptSourceRun(t, second, true)
	f.assertResumeSourceLoad(t, caller, first.ID, *third.CommitSHA)
	f.assertResumeSourceLoad(t, caller, second.ID, "")
	caller.Pod.Status.Phase = core.PodSucceeded
	f.assertResumeSourceLoad(t, caller, first.ID, "")
	caller.Pod.Status.Phase = core.PodRunning
	caller.UID = "replacement"
	f.assertResumeSourceLoad(t, caller, first.ID, "")
	full := f.acceptSourceRun(t, third, false)
	if full.Origin.Resumed() {
		t.Fatal("full rerun retained resume lineage")
	}
	caller.Workflow, caller.UID = full.Origin.WorkflowName, full.Origin.NativeUID()
	f.assertResumeSourceLoad(t, caller, full.ID, *full.CommitSHA)
	f.assertResumeSourceLoad(t, caller, first.ID, "")
	caller.Workflow, caller.UID = first.Origin.WorkflowName, first.Origin.NativeUID()
	f.assertResumeSourceLoad(t, caller, full.ID, "")
}

// Acceptance starts from a sealed source binding; Git staging is injected so
// the HTTP reply identifies exactly which durable run's revision was selected.
func (f deliveryFixture) acceptSourceRun(t *testing.T, old stateport.Run, resumed bool) stateport.Run {
	t.Helper()
	status := stepmodel.RunFailed
	if !resumed {
		status = stepmodel.RunPassed
	}
	if err := f.o.c.store.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: old.ID, Revision: old.Revision, Status: status}); err != nil {
		t.Fatal(err)
	}
	v := old.Admission.Clone()
	v.RunID = old.ID + "-next"
	if resumed {
		v.CommitSHA = strings.Repeat("f", 40)
		if old.Origin.Resumed() {
			v.CommitSHA = strings.Repeat("e", 40)
		}
	}
	v.ObjectSHA = v.CommitSHA
	v.SourceBindings = []sourcecontract.Binding{{Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, Revision: sourcecontract.Revision{Repository: old.Repo, Commit: v.CommitSHA}}}
	p, err := admissioncontract.Seal(v)
	if err != nil {
		t.Fatal(err)
	}
	a := actor.Actor{Identity: old.Origin.Actor, AuthMethod: "cert"}
	ci := model.CIRequest{ReceiveID: v.RunID, Repo: old.Repo, Ref: old.Ref, ObjectSHA: v.ObjectSHA, CommitSHA: &v.CommitSHA, Trigger: v.Trigger, Actor: a}
	input := stateport.Acceptance{Request: ci, Prepared: p, RerunOf: old.ID}
	if resumed {
		w := old.Admission.Workflow.DeepCopy()
		w.Name, w.Labels = old.Origin.WorkflowName, map[string]string{nativebinding.RunLabel: v.RunID}
		body, failure := json.Marshal(nativebinding.ResumePlan{UID: f.w.UID, ResourceVersion: "7", Workflow: w})
		if failure != nil {
			t.Fatal(failure)
		}
		input.Resume = &model.RunOrigin{ResumedFrom: old.ID, ResumeUID: old.Origin.NativeUID(), WorkflowName: old.Origin.WorkflowName, Assisted: true, ResumePlan: body}
		input.RerunOf = ""
	}
	accepted, err := f.o.c.store.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: ci.ReceiveID, Repository: ci.Repo, Actor: a, Runs: []stateport.Acceptance{input}})
	if err != nil {
		t.Fatal(err)
	}
	r := accepted.Runs[0]
	if !resumed {
		f.bindSourceRun(t, r)
	}
	r, err = f.o.c.store.Run(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f deliveryFixture) bindSourceRun(t *testing.T, r stateport.Run) {
	t.Helper()
	v := r.Admission
	b := nativebinding.RunBinding{RunID: r.ID, RepositoryID: r.RepositoryID, Workflow: nativebinding.Object{Namespace: v.Identity.Namespace, Name: r.ID, UID: r.ID + "-uid"}, Revision: sourcecontract.Revision{Repository: r.Repo, Commit: *r.CommitSHA}, Ref: r.Ref, Trigger: string(v.Trigger), Identity: v.Digest, SpecSHA256: v.SpecDigest}
	if err := f.o.c.store.BindWorkflow(t.Context(), stateport.Action{Actor: r.Origin.Actor}, b); err != nil {
		t.Fatal(err)
	}
}

func (f deliveryFixture) assertResumeSourceLoad(t *testing.T, caller sourcedelivery.Caller, locator string, commit string) {
	t.Helper()
	staged := 0
	h := sourcedelivery.New(sourcedelivery.Config{Budgets: &sourcedelivery.Budgets{Exports: 1, Archives: 2, Transfers: 2, Auth: 1, WorkflowWaiters: 16}, Now: f.o.c.clock.Now, Run: f.o.sourceRun,
		Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) { return caller, nil },
		Stage: func(_ context.Context, b sourcecontract.Binding) (sourcedelivery.Staged, error) {
			staged++
			return sourcedelivery.Staged{Body: io.NopCloser(strings.NewReader(b.Revision.Commit)), Bytes: int64(len(b.Revision.Commit))}, nil
		}})
	body, err := json.Marshal(sourcedelivery.LoadRequest{Locator: locator, Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, sourcedelivery.LoadPath, strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer fixture")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if commit == "" {
		if w.Code != http.StatusForbidden || staged != 0 {
			t.Fatal("refused source reached staging", locator, w.Code, staged)
		}
	} else if w.Code != http.StatusOK || w.Body.String() != commit || staged != 1 || w.Header().Get(sourcedelivery.ReceiptTrailer) == "" {
		t.Fatal("locator did not deliver current source", locator, w.Code, w.Body.String(), staged)
	}
}
