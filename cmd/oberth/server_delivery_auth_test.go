package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	auth "k8s.io/api/authentication/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

func TestBrokerIdentityReadsUseDedicatedClients(t *testing.T) {
	f := newDeliveryFixture(t)
	f.authentication(t)
	f.o.c.delivery.Access.Audience = f.o.c.storeWriter.Config.Audience
	f.o.cluster.deliveryReview, f.o.cluster.deliveryKube, f.o.cluster.deliveryArgo = f.o.cluster.Kube, f.o.cluster.Kube, f.o.cluster.Argo
	f.o.cluster.Kube, f.o.cluster.Argo = kubefake.NewClientset(), wffake.NewSimpleClientset()
	caller, err := f.o.brokerAuthentication().Authenticate(t.Context(), f.jwt)
	if err != nil {
		t.Fatal("broker authentication used live observer client", err)
	}
	identity, err := f.o.deliveryIdentity(t.Context(), caller)
	if err != nil || identity.RunID != f.run.ID {
		t.Fatal("broker Workflow identity used live observer client", identity, err)
	}
}

func TestBrokerAuthenticationHasTenSecondDeadline(t *testing.T) {
	f := newDeliveryFixture(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		authenticate := f.o.boundedDeliveryAuthentication(func(ctx context.Context, _ string) (sourcedelivery.Caller, error) {
			<-ctx.Done()
			return sourcedelivery.Caller{}, ctx.Err()
		})
		done := make(chan error, 1)
		go func() { _, err := authenticate(ctx, "192.0.2.1", "unverified"); done <- err }()
		bound := time.NewTimer(11 * time.Second)
		defer bound.Stop()
		select {
		case err := <-done:
			var failure *sourcedelivery.Failure
			if !errors.As(err, &failure) || failure.Code != "unavailable" {
				t.Fatal("authentication deadline was not retryable", err)
			}
		case <-bound.C:
			t.Fatal("authentication exceeded ten-second bound")
		}
	})
}

func TestBrokerIdentityClientsHaveTheirOwnSharedBudget(t *testing.T) {
	t.Parallel()
	c, err := newClusterClients(&rest.Config{Host: "https://cluster.oberth.ci"})
	if err != nil {
		t.Fatal(err)
	}
	review := c.deliveryReview.AuthenticationV1().RESTClient().GetRateLimiter()
	kube := c.deliveryKube.CoreV1().RESTClient().GetRateLimiter()
	argo := c.deliveryArgo.ArgoprojV1alpha1().RESTClient().GetRateLimiter()
	live := c.Kube.CoreV1().RESTClient().GetRateLimiter()
	if kube != argo || kube == live || review == live || review == kube || review.QPS() != deliveryReviewQPS || kube.QPS() != deliveryReadQPS {
		t.Fatal("broker review and verified reads do not have separate budgets")
	}
	acceptBurst(t, review, deliveryReviewBurst, "broker review burst refused early")
	if review.TryAccept() {
		t.Fatal("broker review burst unbounded")
	}
	acceptBurst(t, argo, deliveryReadBurst, "unverified reviews consumed verified read capacity")
	acceptBurst(t, live, serverClientBurst, "broker consumed live observation capacity")
}

func acceptBurst(t *testing.T, limiter flowcontrol.RateLimiter, burst int, refused string) {
	t.Helper()
	for range burst {
		if !limiter.TryAccept() {
			t.Fatal(refused)
		}
	}
}

func TestBrokerAuthenticationFloodKeepsObservationAndDeliveryBounded(t *testing.T) {
	f := newDeliveryFixture(t)
	f.o.settings.SourceClientBurst = 3
	f.o.c.delivery.Access.Audience = f.o.c.storeWriter.Config.Audience
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		var reviews atomic.Int32
		clients, err := newClusterClients(&rest.Config{Host: "https://cluster.oberth.ci", ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}, Transport: brokerClusterTransport(t, f, entered, &reviews)})
		if err != nil {
			t.Fatal(err)
		}
		f.o.cluster = clients
		h := f.o.secretDeliveryHandler(f.o.brokerAuthentication().Authenticate)
		first := make(chan *httptest.ResponseRecorder, 1)
		go func() { first <- deliveryRequest(t, h, "unverified", `{"path":"registry/value"}`) }()
		<-entered
		for range 64 {
			r := deliveryRequest(t, h, "unverified", `{"path":"registry/value"}`)
			if r.Code != 503 || r.Header().Get("Retry-After") != "1" {
				t.Fatal("authentication overflow was not retryable", r.Code)
			}
		}
		if reviews.Load() != 1 {
			t.Fatal("flood reached Kubernetes", reviews.Load())
		}
		assertBrokerFloodObservation(t, f)
		client, err := secretstore.New(secretstore.Connection{URL: "https://broker.invalid", Transport: testTransport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w.Result(), nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
		defer cancel()
		ran := false
		err = client.DeliverBroker(ctx, secretstore.Delivery{Parent: "/dev/shm", Token: []byte(f.jwt), Paths: []string{"registry/value"}, Run: func(context.Context, string) error { ran = true; return nil }})
		if err != nil || !ran {
			t.Fatal("legitimate delivery exceeded its retry bound", err)
		}
		if r := <-first; r.Code != 503 || r.Header().Get("Retry-After") != "1" {
			t.Fatal("stalled authentication exceeded its deadline", r.Code)
		}
	})
}

func brokerClusterTransport(t *testing.T, f deliveryFixture, entered chan struct{}, reviews *atomic.Int32) http.RoundTripper {
	t.Helper()
	return testTransport(func(r *http.Request) (*http.Response, error) {
		var value any
		switch {
		case strings.HasSuffix(r.URL.Path, "/tokenreviews"):
			var review auth.TokenReview
			if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
				t.Error(err)
			}
			reviews.Add(1)
			if review.Spec.Token == "unverified" {
				if reviews.Load() == 1 {
					close(entered)
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			value = &auth.TokenReview{Status: auth.TokenReviewStatus{Authenticated: true, Audiences: review.Spec.Audiences, User: auth.UserInfo{Username: "system:serviceaccount:" + f.pod.Namespace + ":step-account", Extra: map[string]auth.ExtraValue{"authentication.kubernetes.io/pod-name": {f.pod.Name}, "authentication.kubernetes.io/pod-uid": {string(f.pod.UID)}}}}}
		case strings.Contains(r.URL.Path, "/pods/"):
			value = f.pod
		default:
			value = f.w
		}
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
}

func assertBrokerFloodObservation(t *testing.T, f deliveryFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := f.o.cluster.Kube.CoreV1().Pods(f.pod.Namespace).Get(ctx, f.pod.Name, meta.GetOptions{}); err != nil {
		t.Fatal("broker flood delayed live Pod observation", err)
	}
	if _, err := f.o.cluster.Argo.ArgoprojV1alpha1().Workflows(f.w.Namespace).Get(ctx, f.w.Name, meta.GetOptions{}); err != nil {
		t.Fatal("broker flood delayed live Workflow observation", err)
	}
}

func TestBrokerInvalidTokenCoolsDownItsAddress(t *testing.T) {
	f := newDeliveryFixture(t)
	now := time.Unix(1_800_000_000, 0)
	f.o.c.clock.Now = func() time.Time { return now }
	var calls atomic.Int32
	valid := false
	authenticate := f.o.boundedDeliveryAuthentication(func(context.Context, string) (sourcedelivery.Caller, error) {
		calls.Add(1)
		if valid {
			return sourcedelivery.Caller{UID: "pod-uid"}, nil
		}
		return sourcedelivery.Caller{}, &sourcedelivery.Failure{Code: "unauthenticated"}
	})
	try := func(remote string) int32 {
		before := calls.Load()
		_, _ = authenticate(t.Context(), remote, "token")
		return calls.Load() - before
	}
	for _, wait := range []time.Duration{30, 60, 120, 240, 300, 300} {
		if try("10.0.0.1") != 1 {
			t.Fatal("expired cooldown still refused the address", wait*time.Second)
		}
		now = now.Add(wait*time.Second - time.Second)
		if try("10.0.0.1") != 0 || try("10.0.0.2") != 1 {
			t.Fatal("cooldown did not refuse only the address without an API call", wait*time.Second)
		}
		now = now.Add(time.Second)
	}
	valid = true
	if try("10.0.0.1") != 1 {
		t.Fatal("valid token after the cooldown was refused")
	}
	valid = false
	try("10.0.0.1")
	now = now.Add(deliveryCooldown)
	if try("10.0.0.1") != 1 {
		t.Fatal("valid token did not reset the cooldown")
	}
}

// The cooldown names the Pod the watch held at the address when the invalid
// token arrived; it answers the client's retryable 503, never 403.
func TestBrokerCooldownNamesThePodAtItsAddress(t *testing.T) {
	f := newDeliveryFixture(t)
	now := time.Unix(1_800_000_000, 0)
	f.o.c.clock.Now = func() time.Time { return now }
	at := map[string]string{"10.0.0.1": "flooder"}
	f.o.podAt = func(ip string) string { return at[ip] }
	var calls atomic.Int32
	authenticate := f.o.boundedDeliveryAuthentication(func(context.Context, string) (sourcedelivery.Caller, error) {
		calls.Add(1)
		return sourcedelivery.Caller{}, &sourcedelivery.Failure{Code: "unauthenticated"}
	})
	try := func(remote string) (int32, *httptest.ResponseRecorder) {
		before := calls.Load()
		_, err := authenticate(t.Context(), remote, "token")
		w := httptest.NewRecorder()
		secretDeliveryRefusal(w, err)
		return calls.Load() - before, w
	}
	try("10.0.0.1")
	if n, w := try("10.0.0.1"); n != 0 || w.Code != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("cooled Pod was not answered a retryable 503 without an API call", n, w.Code)
	}
	at["10.0.0.1"] = ""
	if n, _ := try("10.0.0.1"); n != 0 {
		t.Fatal("an address without a known Pod escaped its Pod's cooldown")
	}
	at["10.0.0.1"] = "successor"
	if n, _ := try("10.0.0.1"); n != 1 {
		t.Fatal("a newer Pod on a recycled address inherited the cooldown")
	}
	if n, _ := try("10.0.0.1"); n != 0 {
		t.Fatal("the newer Pod's own invalid token was not cooled")
	}
	try("10.0.0.2")
	at["10.0.0.2"] = "later"
	if n, _ := try("10.0.0.2"); n != 0 {
		t.Fatal("an address without a Pod lost its address cooldown")
	}
}

func TestBrokerCooldownTableForgetsOldestWhenFull(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	gate := &deliveryAuthGate{limit: 1, now: func() time.Time { return now }, remotes: map[string]struct{}{}, refused: map[string]deliveryRefusal{}}
	invalid := &sourcedelivery.Failure{Code: "unauthenticated"}
	for n := range deliveryCooldownAddresses + 1 {
		remote := fmt.Sprint("10.1.", n/256, ".", n%256)
		if gate.acquire(remote, "") != nil {
			t.Fatal("fresh address refused", remote)
		}
		gate.release(remote, "", invalid)
		now = now.Add(time.Millisecond)
	}
	_, first := gate.refused["10.1.0.0"]
	_, second := gate.refused["10.1.0.1"]
	if len(gate.refused) != deliveryCooldownAddresses || first || !second {
		t.Fatal("cooldown table did not forget only its oldest address", len(gate.refused))
	}
}
