//go:build linux

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	auth "k8s.io/api/authentication/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktest "k8s.io/client-go/testing"
)

func TestComposedSecretOutageFinishesWithInfrastructureRerun(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	backend := f.secretBackend(t)
	backend.Fail = "/auth/" + f.config.SecretStore.AuthMount + "/login"
	f.prepareSecretCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	remote := f.administratorClient(t)
	var grant apicontract.Grant
	if err := remote.Invoke(t.Context(), "access_allow", apicontract.AccessAllowArgs{Repo: gittest.Repository, Workflow: ".oberth/build.yaml", Step: "build", Secret: "registry/key"}, &grant); err != nil {
		t.Fatal(err)
	}
	f.bindClient(t)
	f.git.Git(t, f.git.Work, "push", "-o", "oberth.run=1", "oberth", "HEAD:refs/heads/secret")
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	f.deliverSource(t, run)
	template := run.Admission.Workflow.Spec.Templates[0]
	env := map[string]string{}
	for _, v := range template.Container.Env {
		env[v.Name] = v.Value
	}
	claims, _ := json.Marshal(map[string]string{"sub": "system:serviceaccount:" + f.config.PipelineNamespace + ":" + template.ServiceAccountName})
	jwt := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	f.prepareComposedSecretRequest(t, run, jwt, env)
	r, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+f.sourceListener.Addr().String()+secretstore.BrokerPath, strings.NewReader(`{"path":"registry/key"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Vault-Token", jwt)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(f.ca) {
		t.Fatal("source trust is invalid")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: f.publicHostname()}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	reply, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = reply.Body.Close()
	if reply.StatusCode != 503 {
		t.Fatal("step store failure was not refused", reply.StatusCode)
	}
	// The composed server uses real time; the native fixture must terminate
	// after this request, rather than at its frozen startup time.
	f.clock.Advance(f.deps.clock.Now().Sub(f.clock.Now()))
	f.endNative(t, run, 1)
	ended := f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunFailed })
	if ended.Failure == nil || ended.Failure.Class != stepmodel.FailureInfrastructure || ended.Failure.Retry.Action != stepmodel.RetryRerun {
		t.Fatal("running server did not use its durable receipt", ended.Failure)
	}
}

// The fixture stands in for the controller's running node and API-server
// authentication; the child makes its secret request through the real listener.
func (f *composedFixture) prepareComposedSecretRequest(t *testing.T, r stateport.Run, jwt string, env map[string]string) {
	t.Helper()
	wc := f.argo.ArgoprojV1alpha1().Workflows(f.config.PipelineNamespace)
	w, err := wc.Get(t.Context(), r.Origin.WorkflowName, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := f.kube.CoreV1().Pods(w.Namespace).Get(t.Context(), w.Name, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w.Status = wf.WorkflowStatus{Phase: wf.WorkflowRunning, Nodes: wf.Nodes{w.Name: {ID: w.Name, Name: pod.Annotations["workflows.argoproj.io/node-name"], TemplateName: r.Admission.Plan.Entries[0].Name, Type: wf.NodeTypePod, Phase: wf.NodeRunning, StartedAt: meta.NewTime(f.clock.Now())}}}
	if _, err = wc.Update(t.Context(), w, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.kube.PrependReactor("create", "tokenreviews", func(a ktest.Action) (bool, runtime.Object, error) {
		review := a.(ktest.CreateAction).GetObject().(*auth.TokenReview)
		return true, &auth.TokenReview{Status: auth.TokenReviewStatus{Authenticated: review.Spec.Token == jwt, Audiences: review.Spec.Audiences, User: auth.UserInfo{Username: "system:serviceaccount:" + pod.Namespace + ":" + pod.Spec.ServiceAccountName, Extra: map[string]auth.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}}}, nil
	})
	var delivery runtimeconfig.SecretDelivery
	if err = json.Unmarshal([]byte(env[runtimeconfig.SecretConfigEnv]), &delivery); err != nil {
		t.Fatal(err)
	}
	if delivery.BrokerAddress == "" || delivery.BrokerCAPEM == "" {
		t.Fatal("admission omitted the private delivery server")
	}
	delivery.BrokerAddress = "https://" + f.sourceListener.Addr().String()
	delivery.BrokerServerName, delivery.BrokerCAPEM = f.publicHostname(), string(f.ca)
	data, err := json.Marshal(delivery)
	if err != nil {
		t.Fatal(err)
	}
	env[runtimeconfig.SecretConfigEnv] = string(data)
}
