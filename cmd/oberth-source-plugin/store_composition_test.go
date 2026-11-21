package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/sourceplugin"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	"k8s.io/utils/clock"
)

type durableSource struct {
	git     *gittest.Fixture
	state   *store.Store
	cache   *gitcache.Cache
	path    string
	binding sourcecontract.Binding
}

func newDurableSource(t *testing.T) *durableSource {
	t.Helper()
	d := &durableSource{git: gittest.New(t), path: filepath.Join(t.TempDir(), "state.sqlite")}
	var err error
	d.state, err = store.Create(t.Context(), d.path, store.DefaultOptions(instant))
	storeOK(t, err)
	t.Cleanup(func() { storeOK(t, d.state.Close()) })
	action := stateport.Action{Actor: "fixture"}
	storeOK(t, d.state.RegisterUpstream(t.Context(), action, store.Upstream{
		Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source",
	}))
	repo, err := d.state.RegisterRepository(t.Context(), action, gittest.Repository)
	storeOK(t, err)
	d.binding = d.git.Binding()
	d.binding.RepositoryID = repo.ID
	p := admissioncontract.PreparedView{
		Workflow: &wfv1.Workflow{}, RunID: "run-1", Repository: repo.Canonical,
		Ref: "refs/heads/main", ObjectSHA: d.git.Commit, CommitSHA: d.git.Commit,
		Trigger: model.TriggerBranch, SourceBindings: []sourcecontract.Binding{d.binding},
	}
	p.Identity.Namespace = "runs"
	sealed, err := admissioncontract.Seal(p)
	storeOK(t, err)
	r := model.CIRequest{ReceiveID: "receive-store", Repo: repo.Canonical, Ref: p.Ref,
		ObjectSHA: p.ObjectSHA, CommitSHA: new(p.CommitSHA), Trigger: p.Trigger}
	r.Actor.Identity, r.Actor.AuthMethod = "fixture", "cert"
	accepted, err := d.state.AcceptBatch(t.Context(), stateport.BatchAcceptance{
		ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor,
		Runs: []stateport.Acceptance{{Request: r, Prepared: sealed}},
	})
	storeOK(t, err)
	if !accepted.Fresh || len(accepted.Runs) != 1 {
		t.Fatal("store did not record one sealed run", accepted)
	}
	d.cache = d.git.Cache(t, d.liveOwners)
	storeOK(t, d.cache.Keep(t.Context(), p.RunID, accepted.Runs[0].Admission.SourceBindings))
	return d
}

func (d *durableSource) liveOwners(ctx context.Context, _ string) (gitcache.OwnerPage, error) {
	r, err := d.state.Run(ctx, "run-1")
	if err != nil || r.Status != stepmodel.RunRunning {
		return gitcache.OwnerPage{}, err
	}
	return gitcache.OwnerPage{Owners: []gitcache.Owner{{ID: r.ID, Bindings: r.Admission.SourceBindings}}}, nil
}

func storedSource(ctx context.Context, port contracttest.RunPort, id string) (sourcedelivery.Run, error) {
	r, err := port.Run(ctx, id)
	return sourcedelivery.Run{
		Admission: r.Admission, Namespace: r.Admission.Identity.Namespace,
		Workflow: r.Origin.WorkflowName, UID: r.Origin.WorkflowUID,
		Terminal: r.Status != stepmodel.RunRunning && r.Status != stepmodel.RunQueued,
	}, err
}

func (d *durableSource) server(caller sourcedelivery.Caller) *sourcedelivery.Server {
	return sourcedelivery.New(sourcedelivery.Config{
		Now: clock.RealClock{}.Now, SourceClientBurst: 30, Stage: d.cache.Stage,
		Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) { return caller, nil },
		Run: func(ctx context.Context, id string) (sourcedelivery.Run, error) {
			return storedSource(ctx, d.state, id)
		},
	})
}

func (d *durableSource) bind(t *testing.T) nativebinding.RunBinding {
	t.Helper()
	r, err := d.state.Run(t.Context(), "run-1")
	storeOK(t, err)
	b := nativebinding.RunBinding{
		RunID: r.ID, RepositoryID: r.RepositoryID, Ref: r.Ref, Trigger: string(r.Origin.Trigger),
		Revision: sourcecontract.Revision{Repository: r.Repo, Commit: *r.CommitSHA},
		Identity: r.Admission.Digest, SpecSHA256: r.Admission.SpecDigest,
		Workflow: nativebinding.Object{Namespace: "runs", Name: "stored-workflow", UID: "stored-uid"},
	}
	storeOK(t, d.state.BindWorkflow(t.Context(), stateport.Action{Actor: "fixture"}, b))
	return b
}

func TestStoredAdmissionReopensAndDeliversExactSourceThroughPlugin(t *testing.T) {
	d := newDurableSource(t)
	b := d.bind(t)
	storeOK(t, d.state.Close())
	var err error
	state, err := store.Open(t.Context(), d.path, store.DefaultOptions(instant))
	storeOK(t, err)
	d.state = state
	r, err := d.state.Run(t.Context(), "run-1")
	storeOK(t, err)
	if r.Admission.Digest != b.Identity || r.Admission.SourceBindings[0].Revision.Commit != d.git.Commit {
		t.Fatal("reopen lost the admitted source or digest")
	}
	storeOK(t, d.cache.Keep(t.Context(), r.ID, r.Admission.SourceBindings))
	d.git.Write(t, "file.txt", []byte("new upstream source\n"))
	d.git.CommitFile(t, "advance upstream")
	d.git.Git(t, d.git.Work, "push", d.git.Bare, "HEAD:refs/heads/main")
	gittest.ExpireRefresh(t, d.git.Store())
	storeOK(t, d.cache.Fetch(t.Context(), gittest.Repository))
	storeOK(t, d.cache.GC(t.Context()))
	caller := sourcedelivery.Caller{Namespace: b.Workflow.Namespace, Workflow: b.Workflow.Name, UID: b.Workflow.UID}
	client, err := sourceplugin.New(pluginConfig(t, d.server(caller)))
	storeOK(t, err)
	defer client.Close()
	descriptor, err := json.Marshal(d.binding.Descriptor)
	storeOK(t, err)
	input := &artifact.Artifact{Plugin: &artifact.PluginArtifact{
		Name: "oberth", Key: r.ID, Configuration: string(descriptor),
	}}
	path := filepath.Join(t.TempDir(), "source.tgz")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	storeOK(t, client.Load(ctx, input, path))
	checkSourceArchive(t, path)
	checkStoredDigest(t, d.server(caller), sourcedelivery.LoadRequest{
		Locator: r.ID, Descriptor: d.binding.Descriptor,
	}, r.Admission.Digest)
}

func TestStoredWorkflowBindingAndTerminalStateFenceDelivery(t *testing.T) {
	d := newDurableSource(t)
	caller := sourcedelivery.Caller{Namespace: "runs", Workflow: "stored-workflow", UID: "stored-uid"}
	request := sourcedelivery.LoadRequest{Locator: "run-1", Descriptor: d.binding.Descriptor}
	checkStoredLoad(t, d.server(caller), request, http.StatusServiceUnavailable)
	b := d.bind(t)
	b.Workflow.UID = "replacement"
	if err := d.state.BindWorkflow(t.Context(), stateport.Action{Actor: "fixture"}, b); err == nil {
		t.Fatal("store replaced the recorded Workflow UID")
	}
	wrong := caller
	wrong.UID = b.Workflow.UID
	checkStoredLoad(t, d.server(wrong), request, http.StatusForbidden)
	checkStoredLoad(t, d.server(caller), request, http.StatusOK)
	r, err := d.state.Run(t.Context(), "run-1")
	storeOK(t, err)
	storeOK(t, d.state.Finish(t.Context(), stateport.Action{Actor: "fixture"}, stateport.RunUpdate{
		ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed,
	}))
	checkStoredLoad(t, d.server(caller), request, http.StatusForbidden)
	storeOK(t, d.cache.GC(t.Context()))
	refs := d.git.Git(t, d.git.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/")
	if refs != "" {
		t.Fatal("finished stored run retained source", refs)
	}
	storeOK(t, d.state.Close())
	checkStoredLoad(t, d.server(caller), request, http.StatusServiceUnavailable)
}

func checkStoredLoad(t *testing.T, server http.Handler, request sourcedelivery.LoadRequest, status int) {
	t.Helper()
	response := load(t, server, request, "Bearer bound-token")
	if response.Code != status {
		t.Fatalf("stored load: %d, want %d: %s", response.Code, status, response.Body.String())
	}
}

func checkStoredDigest(t *testing.T, server http.Handler, request sourcedelivery.LoadRequest, digest string) {
	t.Helper()
	response := load(t, server, request, "Bearer bound-token").Result()
	defer func() { _ = response.Body.Close() }()
	raw, err := base64.RawURLEncoding.DecodeString(response.Trailer.Get(sourcedelivery.ReceiptTrailer))
	storeOK(t, err)
	var receipt sourcedelivery.LoadReceipt
	storeOK(t, json.Unmarshal(raw, &receipt))
	if receipt.Stream.AdmissionDigest != digest {
		t.Fatal("load receipt differs from the saved admission", receipt.Stream.AdmissionDigest, digest)
	}
}

func storeOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
