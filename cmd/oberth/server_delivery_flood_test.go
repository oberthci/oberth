package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	"github.com/oberthci/oberth/internal/secretstore"
	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ktest "k8s.io/client-go/testing"
)

// The cluster side is a real rate-limited rest client; only the socket is in
// memory, so the bubble's clock measures the deadlines exactly.
func TestBrokerGarbageFloodLeavesLegitimateDeliveriesWithinDeadline(t *testing.T) {
	f, load := floodFixture(t, 30)
	synctest.Test(t, func(t *testing.T) {
		var reviews atomic.Int32
		h := floodHandler(t, f, load.pods, &reviews)
		stop := garbageFlood(h, []string{"10.0.9.9"})
		// Thirty-two reviews and the flood's single queued place fit in about
		// six seconds of the review budget; verified reads add no wait.
		deliverAllWithin(t, h, load, 10*time.Second)
		stop()
		if reviews.Load() > 50 {
			t.Fatal("garbage flood reached TokenReview beyond its share", reviews.Load())
		}
	})
}

// One run fans out more flooding Pods than there are authentication places;
// each has its own address. Their invalid tokens put each address on cooldown,
// so the real steps' reviews are not starved.
func TestBrokerManyAddressFloodLeavesLegitimateDeliveriesWithinDeadline(t *testing.T) {
	f, load := floodFixture(t, 96)
	synctest.Test(t, func(t *testing.T) {
		f.o.c.clock.Now = time.Now //nolint:forbidigo // synctest owns this clock, so cooldowns expire in the bubble.
		at := map[string]string{}
		for _, p := range load.pods {
			at[p.Name] = string(p.UID)
		}
		remotes := make([]string, 40)
		for n := range remotes {
			remotes[n] = fmt.Sprintf("10.0.9.%d", n)
			at[remotes[n]] = fmt.Sprint("flooder-", n)
		}
		f.o.podAt = func(ip string) string { return at[ip] }
		var reviews atomic.Int32
		h := floodHandler(t, f, load.pods, &reviews)
		stop := garbageFlood(h, remotes)
		<-time.After(3 * time.Second)
		deliverAllWithin(t, h, load, 30*time.Second)
		stop()
		if reviews.Load() > 40+32+8 {
			t.Fatal("garbage flood reached TokenReview beyond its cooldown", reviews.Load())
		}
	})
}

// A flooding Pod exits and a real step's Pod is given its address while the
// address is still cooled; the step's deliveries succeed within its budget.
func TestBrokerRecycledFlooderAddressDeliversToTheNextPod(t *testing.T) {
	f, load := floodFixture(t, 30)
	synctest.Test(t, func(t *testing.T) {
		f.o.c.clock.Now = time.Now //nolint:forbidigo // synctest owns this clock, so cooldowns expire in the bubble.
		var successor atomic.Bool
		f.o.podAt = func(ip string) string {
			if ip == "step-pod-0" && !successor.Load() {
				return "flooder"
			}
			return string(load.pods[f.jwt+"0"].UID)
		}
		var reviews atomic.Int32
		h := floodHandler(t, f, load.pods, &reviews)
		stop := garbageFlood(h, []string{"step-pod-0"})
		<-time.After(31 * time.Second)
		stop()
		successor.Store(true)
		deliverAllWithin(t, h, load, 30*time.Second)
	})
}

type floodLoad struct {
	pods  map[string]*core.Pod
	paths []string
}

func floodFixture(t *testing.T, burst int) (deliveryFixture, floodLoad) {
	f := newDeliveryFixture(t)
	f.o.settings.SourceClientBurst = burst
	f.o.c.delivery.Access.Audience = f.o.c.storeWriter.Config.Audience
	paths := []string{"registry/value", "registry/one", "registry/two", "registry/three"}
	for _, p := range paths {
		f.backend.Resources["/"+f.o.c.storeWriter.Config.Mount+"/steps/step-account/"+p] = map[string]any{"value": base64.StdEncoding.EncodeToString([]byte("private-fixture"))}
	}
	pods := map[string]*core.Pod{}
	for i := range 8 {
		id, name := fmt.Sprintf("node-%d", i), fmt.Sprintf("%s.step-%d", f.w.Name, i)
		f.w.Status.Nodes[id] = wf.NodeStatus{ID: id, Name: name, TemplateName: "build", Type: wf.NodeTypePod, Phase: wf.NodeRunning}
		pod := f.pod.DeepCopy()
		pod.Name, pod.UID = fmt.Sprintf("step-pod-%d", i), types.UID(fmt.Sprintf("pod-uid-%d", i))
		pod.Annotations = map[string]string{"workflows.argoproj.io/node-id": id, "workflows.argoproj.io/node-name": name}
		pods[f.jwt+fmt.Sprint(i)] = pod
	}
	return f, floodLoad{pods: pods, paths: paths}
}

func floodHandler(t *testing.T, f deliveryFixture, pods map[string]*core.Pod, reviews *atomic.Int32) http.Handler {
	clients, err := newClusterClients(&rest.Config{Host: "https://cluster.oberth.ci", ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}, Transport: stepPodsAPI(f, pods, reviews)})
	if err != nil {
		t.Fatal(err)
	}
	f.o.cluster = clients
	return f.o.secretDeliveryHandler(f.o.brokerAuthentication().Authenticate)
}

// Each flooding address sends a garbage token every 50 ms until stopped.
func garbageFlood(h http.Handler, remotes []string) func() {
	flood, stop := context.WithCancel(context.Background())
	var garbage sync.WaitGroup
	for _, remote := range remotes {
		garbage.Go(func() {
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-flood.Done():
					return
				case <-tick.C:
					garbage.Go(func() {
						r := httptest.NewRequestWithContext(flood, "POST", secretstore.BrokerPath, strings.NewReader(`{"path":"registry/value"}`))
						r.RemoteAddr, r.Header["X-Vault-Token"] = remote+":4000", []string{"garbage"}
						h.ServeHTTP(httptest.NewRecorder(), r)
					})
				}
			}
		})
	}
	return func() { stop(); garbage.Wait() }
}

func deliverAllWithin(t *testing.T, h http.Handler, load floodLoad, budget time.Duration) {
	bound := time.NewTimer(budget)
	defer bound.Stop()
	var steps sync.WaitGroup
	for token, pod := range load.pods {
		steps.Go(func() { deliverFromPod(t, h, floodStep{token: token, remote: pod.Name}, load.paths) })
	}
	done := make(chan struct{})
	go func() { steps.Wait(); close(done) }()
	select {
	case <-done:
	case <-bound.C:
		t.Error("legitimate deliveries were throttled by the flood")
		<-done
	}
}

type floodStep struct{ token, remote string }

func deliverFromPod(t *testing.T, h http.Handler, step floodStep, paths []string) {
	client, err := secretstore.New(secretstore.Connection{URL: "https://broker.invalid", Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.RemoteAddr = step.remote + ":4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Result(), nil
	})})
	if err != nil {
		t.Error(err)
		return
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ran := false
	err = client.DeliverBroker(ctx, secretstore.Delivery{Parent: "/dev/shm", Token: []byte(step.token), Paths: paths, Run: func(context.Context, string) error { ran = true; return nil }})
	if err != nil || !ran {
		t.Error("legitimate delivery missed its deadline under a flood", step.remote, err)
	}
}

func stepPodsAPI(f deliveryFixture, pods map[string]*core.Pod, reviews *atomic.Int32) http.RoundTripper {
	byName := map[string]*core.Pod{}
	for _, p := range pods {
		byName[p.Name] = p
	}
	return testTransport(func(r *http.Request) (*http.Response, error) {
		var value any = f.w
		switch {
		case strings.HasSuffix(r.URL.Path, "/tokenreviews"):
			reviews.Add(1)
			var review auth.TokenReview
			if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
				return nil, err
			}
			p, ok := pods[review.Spec.Token]
			value = &auth.TokenReview{}
			if ok {
				value = &auth.TokenReview{Status: auth.TokenReviewStatus{Authenticated: true, Audiences: review.Spec.Audiences, User: auth.UserInfo{Username: "system:serviceaccount:" + p.Namespace + ":step-account", Extra: map[string]auth.ExtraValue{"authentication.kubernetes.io/pod-name": {p.Name}, "authentication.kubernetes.io/pod-uid": {string(p.UID)}}}}}
			}
		case strings.Contains(r.URL.Path, "/pods/"):
			value = byName[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
		}
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
}

func TestBrokerVerifiedReadIsSingleAndRetryable(t *testing.T) {
	f := newDeliveryFixture(t)
	var gets atomic.Int32
	var refuse atomic.Bool
	f.o.cluster.Argo.(*wffake.Clientset).PrependReactor("get", "workflows", func(ktest.Action) (bool, runtime.Object, error) {
		gets.Add(1)
		if refuse.Load() {
			return true, nil, apierrors.NewTooManyRequests("busy", 1)
		}
		return false, nil, nil
	})
	h := f.handler(t)
	if r := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`); r.Code != 200 || gets.Load() != 1 {
		t.Fatal("verified delivery read its Workflow more than once", r.Code, gets.Load())
	}
	refuse.Store(true)
	if r := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`); r.Code != 503 || r.Header().Get("Retry-After") != "1" {
		t.Fatal("transient identity read was not retryable", r.Code)
	}
	refuse.Store(false)
	f.o.cluster.Argo.(*wffake.Clientset).PrependReactor("get", "workflows", func(ktest.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, f.w.Name)
	})
	if r := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`); r.Code == 503 {
		t.Fatal("a missing Workflow was retried")
	}
}
