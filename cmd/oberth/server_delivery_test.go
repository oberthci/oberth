package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktest "k8s.io/client-go/testing"
)

type deliveryFixture struct {
	o       *serverOwners
	run     stateport.Run
	w       *wf.Workflow
	pod     *core.Pod
	backend *storefake.Store
	jwt     string
	logDir  string
}

func newDeliveryFixture(t *testing.T) deliveryFixture {
	t.Helper()
	c := testComposition(t)
	db, err := store.Create(t.Context(), filepath.Join(t.TempDir(), "state.db"), store.DefaultOptions(c.clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c.store = db
	repo := storetest.Repository(t, db)
	sha := strings.Repeat("a", 40)
	request := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeInspect, Repository: repo.Canonical, RunID: "run-delivery", Ref: "refs/heads/main", ObjectSHA: sha, CommitSHA: sha, FileBlob: sha, Trigger: model.TriggerBranch, SelectedFile: ".oberth/build.yaml", Document: []byte(starterWorkflow()), IdentityRequest: admissioncontract.IdentityRequest{Namespace: c.config.PipelineNamespace}}
	prepared, err := argoworkflow.Prepare(t.Context(), request, argoworkflow.Policy{}, admissioncontract.PrepareHooks{OfflineValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	a := actor.Actor{Identity: "operator", AuthMethod: "cert"}
	ci := model.CIRequest{ReceiveID: "delivery", Repo: repo.Canonical, Ref: request.Ref, ObjectSHA: sha, CommitSHA: &sha, Trigger: model.TriggerBranch, Actor: a}
	accepted, err := db.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: ci.ReceiveID, Repository: ci.Repo, Actor: a, Runs: []stateport.Acceptance{{Request: ci, Prepared: prepared}}})
	if err != nil {
		t.Fatal(err)
	}
	r := accepted.Runs[0]
	b := nativebinding.RunBinding{RunID: r.ID, RepositoryID: r.RepositoryID, Workflow: nativebinding.Object{Namespace: c.config.PipelineNamespace, Name: r.ID, UID: "workflow-uid"}, Revision: sourcecontract.Revision{Repository: r.Repo, Commit: *r.CommitSHA}, Ref: r.Ref, Trigger: string(r.Origin.Trigger), Identity: r.Admission.Digest, SpecSHA256: r.Admission.SpecDigest}
	if err = db.BindWorkflow(t.Context(), stateport.Action{Actor: a.Identity}, b); err != nil {
		t.Fatal(err)
	}
	w := r.Admission.Workflow.DeepCopy()
	w.Name, w.Namespace, w.UID = r.ID, b.Workflow.Namespace, "workflow-uid"
	w.Labels = map[string]string{nativebinding.EngineLabel: nativebinding.EngineValue, nativebinding.RunLabel: r.ID}
	w.Annotations = map[string]string{nativebinding.RunIDAnnotation: r.ID}
	w.Status = wf.WorkflowStatus{Phase: wf.WorkflowRunning, Nodes: wf.Nodes{"node": {ID: "node", Name: w.Name, TemplateName: "build", Type: wf.NodeTypePod, Phase: wf.NodeRunning}}}
	controller := true
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "step-pod", Namespace: w.Namespace, UID: "pod-uid", Labels: map[string]string{"workflows.argoproj.io/workflow": w.Name}, Annotations: map[string]string{"workflows.argoproj.io/node-id": "node", "workflows.argoproj.io/node-name": w.Name}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: w.Name, UID: w.UID, Controller: &controller}}}, Spec: core.PodSpec{ServiceAccountName: "step-account"}}
	logDir := t.TempDir()
	c.logs, err = runlog.Open(t.Context(), logDir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.logs.Close() })
	backend := &storefake.Store{Initialized: true, Resources: map[string]map[string]any{"/" + storeConfiguration(c.config).Mount + "/steps/step-account/registry/value": {"value": base64.StdEncoding.EncodeToString([]byte("private-fixture"))}}}
	c.storeWriter.Client, err = secretstore.New(secretstore.Connection{URL: "https://store.invalid", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	c.storeWriter.Config = storeConfiguration(c.config)
	claims, _ := json.Marshal(map[string]string{"sub": "system:serviceaccount:" + w.Namespace + ":step-account"})
	jwt := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	o := &serverOwners{c: c, cluster: clusterClients{Argo: wffake.NewSimpleClientset(w), Kube: kubefake.NewClientset(pod)}}
	return deliveryFixture{o: o, run: r, w: w, pod: pod, backend: backend, jwt: jwt, logDir: logDir}
}

func (f deliveryFixture) authentication(t *testing.T) sourcedelivery.KubernetesAuth {
	t.Helper()
	kube := f.o.cluster.Kube.(*kubefake.Clientset)
	audience := f.o.c.storeWriter.Config.Audience
	kube.PrependReactor("create", "tokenreviews", func(a ktest.Action) (bool, runtime.Object, error) {
		r := a.(ktest.CreateAction).GetObject().(*auth.TokenReview)
		if len(r.Spec.Audiences) != 1 || r.Spec.Audiences[0] != audience {
			t.Error("store audience was not reviewed")
		}
		return true, &auth.TokenReview{Status: auth.TokenReviewStatus{Authenticated: r.Spec.Token == f.jwt, Audiences: []string{audience}, User: auth.UserInfo{Username: "system:serviceaccount:" + f.pod.Namespace + ":step-account", Extra: map[string]auth.ExtraValue{"authentication.kubernetes.io/pod-name": {f.pod.Name}, "authentication.kubernetes.io/pod-uid": {string(f.pod.UID)}}}}}, nil
	})
	a := sourcedelivery.KubernetesAuth{Audience: audience, Review: func(ctx context.Context, r *auth.TokenReview) (*auth.TokenReview, error) {
		return kube.AuthenticationV1().TokenReviews().Create(ctx, r, meta.CreateOptions{})
	}, Pod: func(ctx context.Context, ns, name string) (*core.Pod, error) {
		return kube.CoreV1().Pods(ns).Get(ctx, name, meta.GetOptions{})
	}}
	return a
}

func (f deliveryFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	return f.o.secretDeliveryHandler(f.authentication(t).Authenticate)
}

func deliveryRequest(t *testing.T, handler http.Handler, jwt, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), "POST", secretstore.BrokerPath, strings.NewReader(body))
	r.Header.Set("X-Vault-Token", jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestSecretRequestStoreOutageWritesDurableInfrastructureReceipt(t *testing.T) {
	for _, failure := range []string{"connect", "sealed", "database", "invalid-response"} {
		t.Run(failure, func(t *testing.T) {
			f := newDeliveryFixture(t)
			f.fail(t, failure)
			reply := deliveryRequest(t, f.handler(t), f.jwt, `{"path":"registry/value"}`)
			status := 502
			if failure == "sealed" {
				status = 503
			}
			if reply.Code != status || strings.Contains(reply.Body.String(), "private-fixture") {
				t.Fatal("outage not refused safely", reply.Code)
			}
			reopened, err := runlog.Open(t.Context(), f.logDir, 4096)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			receipts, err := reopened.Infrastructure(t.Context(), f.run.ID)
			if err != nil || len(receipts) != 1 {
				t.Fatal("durable receipt missing", receipts, err)
			}
			f.assertDiagnosis(t, receipts)
		})
	}
}

func (f deliveryFixture) fail(t *testing.T, failure string) {
	t.Helper()
	switch failure {
	case "connect":
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.invalid", Transport: testTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })})
		if err != nil {
			t.Fatal(err)
		}
		f.o.c.storeWriter.Client = c
	case "sealed":
		f.backend.Sealed = true
	case "invalid-response":
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.invalid", Transport: testTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		f.o.c.storeWriter.Client = c
	case "database":
		if err := f.o.c.store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func (f deliveryFixture) assertDiagnosis(t *testing.T, receipts []runlog.InfrastructureReceipt) {
	t.Helper()
	w, pod := f.w.DeepCopy(), f.pod.DeepCopy()
	w.Status.Phase = wf.WorkflowFailed
	n := w.Status.Nodes["node"]
	n.Phase = wf.NodeFailed
	w.Status.Nodes["node"] = n
	pod.Status.ContainerStatuses = []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1}}}}
	o, err := argojob.Project(t.Context(), argojob.Projection{RunID: f.run.ID, Plan: f.run.Admission.Plan, Workflow: w, Pods: []*core.Pod{pod}, Infrastructure: receipts})
	if err != nil || o.Failure == nil || o.Failure.Class != stepmodel.FailureInfrastructure || o.Failure.Retry.Action != stepmodel.RetryRerun {
		t.Fatal("server outage did not classify infrastructure/rerun", o.Failure, err)
	}
}

func TestSecretDeliveryClientsCannotSubmitInfrastructureReceipts(t *testing.T) {
	f := newDeliveryFixture(t)
	h := f.handler(t)
	for _, body := range []string{`{"kind":"secret","pod_uid":"pod-uid","path":"registry/value"}`, `{"path":"../value"}`} {
		if w := deliveryRequest(t, h, f.jwt, body); w.Code != 400 {
			t.Fatal("client-supplied receipt accepted", w.Code)
		}
	}
	if w := deliveryRequest(t, h, "unverified", `{"path":"registry/value"}`); w.Code != 403 {
		t.Fatal("unverified caller accepted", w.Code)
	}
	if w := deliveryRequest(t, h, f.jwt, `{"path":"missing/value"}`); w.Code != 404 {
		t.Fatal("missing value misclassified", w.Code)
	}
	if w := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`); w.Code != 200 {
		t.Fatal("healthy delivery failed", w.Code)
	}
	login := f.backend.Resources["/auth/"+f.o.c.storeWriter.Config.AuthMount+"/login"]
	if login["jwt"] != f.jwt || login["role"] != f.o.c.storeWriter.Config.StepRole || !f.backend.Revoked {
		t.Fatal("broker widened the requesting identity or retained its session")
	}
	r, err := f.o.c.logs.Infrastructure(t.Context(), f.run.ID)
	if err != nil || len(r) != 0 {
		t.Fatal("client request minted receipt", r, err)
	}
}

func TestUserListenerCannotWriteInfrastructureReceipts(t *testing.T) {
	f := newDeliveryFixture(t)
	for _, path := range []string{secretstore.BrokerPath, "/api/infrastructure", "/api/runs/" + f.run.ID + "/infrastructure"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"kind":"secret"}`))
		r.Header.Set("Authorization", "Bearer authenticated-client")
		w := httptest.NewRecorder()
		f.o.c.api.Handler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal("client listener exposed receipt writer", path, w.Code)
		}
	}
	r, err := f.o.c.logs.Infrastructure(t.Context(), f.run.ID)
	if err != nil || len(r) != 0 {
		t.Fatal("client request minted receipt", r, err)
	}
}

func TestSecretDeliveryRequiresLivePodUIDAndWorkflowOwner(t *testing.T) {
	for _, change := range []string{"replacement", "foreign-workflow", "finished"} {
		t.Run(change, func(t *testing.T) {
			f := newDeliveryFixture(t)
			h := f.handler(t)
			pod := f.pod.DeepCopy()
			switch change {
			case "finished":
				w := f.w.DeepCopy()
				w.Status.Phase = wf.WorkflowSucceeded
				if _, err := f.o.cluster.Argo.ArgoprojV1alpha1().Workflows(w.Namespace).Update(t.Context(), w, meta.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				pod.UID = types.UID("replacement")
			default:
				pod.OwnerReferences[0].UID = "foreign"
			}
			if _, err := f.o.cluster.Kube.CoreV1().Pods(pod.Namespace).Update(t.Context(), pod, meta.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			f.backend.Sealed = true
			w := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`)
			if w.Code != 403 {
				t.Fatal("stale identity reached delivery", w.Code)
			}
			r, err := f.o.c.logs.Infrastructure(t.Context(), f.run.ID)
			if err != nil || len(r) != 0 {
				t.Fatal("stale identity minted receipt", r, err)
			}
		})
	}
}
