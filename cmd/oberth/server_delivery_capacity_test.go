package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type stalledDeliveryBody struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (b stalledDeliveryBody) Read([]byte) (int, error) {
	b.entered <- struct{}{}
	<-b.release
	return 0, io.EOF
}
func (stalledDeliveryBody) Close() error { return nil }

func TestSlowUnauthenticatedOtherNamespaceRequestsDoNotTakeDeliverySlots(t *testing.T) {
	f := newDeliveryFixture(t)
	f.o.settings.SourceClientBurst = 3
	h := f.handler(t)
	entered, release, done := make(chan struct{}, 33), make(chan struct{}), make(chan struct{}, 33)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer close(release)
	for range 33 {
		workers.Go(func() {
			r := httptest.NewRequestWithContext(t.Context(), "POST", secretstore.BrokerPath, stalledDeliveryBody{entered, release})
			r.Header.Set("X-Vault-Token", "unverified-other-namespace")
			h.ServeHTTP(httptest.NewRecorder(), r)
			done <- struct{}{}
		})
		// The old code returns from overflow without reading. Both outcomes
		// allow this test to reach the legitimate request and decide there.
		select {
		case <-entered:
		case <-done:
		}
	}
	if reply := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`); reply.Code != 200 {
		t.Fatal("unauthenticated bodies blocked verified delivery", reply.Code)
	}
}

func TestSlowTokenReviewsDoNotTakeDeliverySlots(t *testing.T) {
	f := newDeliveryFixture(t)
	f.o.settings.SourceClientBurst = 3
	authenticate := f.authentication(t).Authenticate
	entered, release, done := make(chan struct{}, 2), make(chan struct{}), make(chan struct{}, 2)
	h := f.o.secretDeliveryHandler(func(ctx context.Context, jwt string) (sourcedelivery.Caller, error) {
		if jwt == "unverified" {
			entered <- struct{}{}
			<-release
			return sourcedelivery.Caller{}, &sourcedelivery.Failure{Code: "unauthenticated"}
		}
		return authenticate(ctx, jwt)
	})
	var workers sync.WaitGroup
	defer workers.Wait()
	defer close(release)
	for range 2 {
		workers.Go(func() { deliveryRequest(t, h, "unverified", `{"path":"registry/value"}`); done <- struct{}{} })
		select {
		case <-entered:
		case <-done:
		}
	}
	if reply := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`); reply.Code != 200 {
		t.Fatal("unverified reviews blocked verified delivery", reply.Code)
	}
}

func TestSecretDeliveryCapacityRefusalRetriesThenSucceeds(t *testing.T) {
	f := newDeliveryFixture(t)
	f.o.settings.SourceClientBurst = 3
	entered, release := make(chan struct{}), make(chan struct{})
	f.o.c.storeWriter.Client = capacityBlockingStore(t, f, entered, release)
	h := f.handler(t)
	first := make(chan int, 1)
	var workers sync.WaitGroup
	defer workers.Wait()
	workers.Go(func() { first <- deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`).Code })
	select {
	case <-entered:
	case code := <-first:
		t.Fatal("first request did not reach delivery", code)
	}
	var retry sync.Once
	defer retry.Do(func() { close(release) })
	client, err := secretstore.New(secretstore.Connection{URL: "https://broker.invalid", Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == 503 && w.Header().Get("Retry-After") == "1" {
			retry.Do(func() { close(release) })
		}
		return w.Result(), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// A file path is not needed: Fetch through DeliverBroker reaches the
	// actual capacity response and retries before calling Run.
	run := false
	err = client.DeliverBroker(t.Context(), secretstore.Delivery{Parent: "/dev/shm", Token: []byte(f.jwt), Paths: []string{"registry/value"}, Run: func(context.Context, string) error { run = true; return nil }})
	if err != nil || !run {
		t.Fatal("capacity refusal killed the step", err)
	}
	if code := <-first; code != 200 {
		t.Fatal("first delivery failed", code)
	}
	if r, err := f.o.c.logs.Infrastructure(t.Context(), f.run.ID); err != nil || len(r) != 0 {
		t.Fatal("capacity minted receipt", r, err)
	}
}

func capacityBlockingStore(t *testing.T, f deliveryFixture, entered, release chan struct{}) *secretstore.Client {
	t.Helper()
	var once sync.Once
	c, err := secretstore.New(secretstore.Connection{URL: "https://store.invalid", Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(entered); <-release })
		return f.backend.RoundTrip(r)
	})})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSecretDeliveryCapacityIsBoundedPerPodAndRun(t *testing.T) {
	s := deliverySlots{limit: 8, pods: map[string]int{}, runs: map[string]int{}}
	r := runlog.InfrastructureReceipt{RunID: "one", PodUID: "one"}
	if !s.acquire(r) || s.acquire(r) {
		t.Fatal("Pod capacity unbounded")
	}
	for _, uid := range []string{"two", "three", "four"} {
		r.PodUID = uid
		if !s.acquire(r) {
			t.Fatal("run refused early")
		}
	}
	r.PodUID = "five"
	if s.acquire(r) {
		t.Fatal("run capacity unbounded")
	}
	r.RunID = "another"
	if !s.acquire(r) {
		t.Fatal("one run monopolized global capacity")
	}
	s.release(r)
	if !s.acquire(r) {
		t.Fatal("released capacity leaked")
	}
	for _, id := range []string{"six", "seven", "eight"} {
		r.RunID, r.PodUID = id, id
		if !s.acquire(r) {
			t.Fatal("global cap refused early")
		}
	}
	r.RunID, r.PodUID = "nine", "nine"
	if s.acquire(r) {
		t.Fatal("global capacity unbounded")
	}
}

func TestSecretDeliveryRejectsWorkflowOutsidePipelineNamespace(t *testing.T) {
	f := newDeliveryFixture(t)
	w, p := f.w.DeepCopy(), f.pod.DeepCopy()
	w.Namespace, p.Namespace = "another-namespace", "another-namespace"
	if _, err := f.o.cluster.Argo.ArgoprojV1alpha1().Workflows(w.Namespace).Create(t.Context(), w, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	who := sourcedelivery.Caller{Namespace: w.Namespace, Workflow: w.Name, UID: string(w.UID), Pod: p}
	if _, err := f.o.deliveryIdentity(t.Context(), who); err == nil {
		t.Fatal("foreign namespace selected a delivery identity")
	}
	h := f.o.secretDeliveryHandler(func(context.Context, string) (sourcedelivery.Caller, error) { return who, nil })
	if reply := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`); reply.Code != 403 || strings.Contains(reply.Body.String(), "private-fixture") {
		t.Fatal("foreign namespace reached delivery", reply.Code)
	}
}
