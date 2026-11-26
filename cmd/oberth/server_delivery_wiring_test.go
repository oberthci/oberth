//go:build linux

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestComposedBrokerSharesDualStackPodAuthenticationSlot(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "dual-stack", Namespace: f.config.PipelineNamespace, UID: "dual-stack-uid", Labels: map[string]string{"workflows.argoproj.io/workflow": "fixture"}}, Status: core.PodStatus{PodIP: "10.0.0.1", PodIPs: []core.PodIP{{IP: "10.0.0.1"}, {IP: "fd00::1"}}}}
	if _, err := f.kube.CoreV1().Pods(pod.Namespace).Create(t.Context(), pod, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	gate := stalledBrokerReview{entered: make(chan struct{}), release: make(chan struct{})}
	entered, release, reviews := gate.entered, gate.release, &gate.reviews
	wireStalledBrokerReview(t, f, &gate)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	assertBrokerPodLookup(t, f.owners, pod)
	first := make(chan int, 1)
	var workers sync.WaitGroup
	unblock := sync.OnceFunc(func() { close(release) })
	defer workers.Wait()
	defer unblock()
	workers.Go(func() { first <- composedBrokerRequest(t, f, "10.0.0.1:1000").Code })
	<-entered
	second := composedBrokerRequest(t, f, "[fd00::1]:1000")
	if second.Code != 503 || second.Header().Get("Retry-After") != "1" || reviews.Load() != 1 {
		t.Fatal("second IP stack took another Pod authentication slot", second.Code, reviews.Load())
	}
	if other := composedBrokerRequest(t, f, "10.0.0.2:1000"); other.Code != 403 || reviews.Load() != 2 {
		t.Fatal("one Pod blocked an unrelated address", other.Code, reviews.Load())
	}
	unblock()
	if code := <-first; code != 403 {
		t.Fatal("invalid token was not refused", code)
	}
	if cooled := composedBrokerRequest(t, f, "10.0.0.1:1000"); cooled.Code != 503 || reviews.Load() != 2 {
		t.Fatal("invalid-token cooldown was not wired", cooled.Code, reviews.Load())
	}
}

func assertBrokerPodLookup(t *testing.T, owner *serverOwners, pod *core.Pod) {
	t.Helper()
	if owner.podAt == nil || owner.podAt("10.0.0.1") != string(pod.UID) || owner.podAt("fd00::1") != string(pod.UID) {
		t.Fatal("native watch did not wire both Pod addresses before source startup")
	}
}

type stalledBrokerReview struct {
	entered, release chan struct{}
	reviews          atomic.Int32
}

func wireStalledBrokerReview(t *testing.T, f *composedFixture, gate *stalledBrokerReview) {
	t.Helper()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: "https://cluster.oberth.ci", Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if gate.reviews.Add(1) == 1 {
			close(gate.entered)
			select {
			case <-gate.release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":{"authenticated":false}}`))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	build := f.deps.constructors.SourceNew
	f.deps.constructors.SourceNew = func(ctx context.Context, c *composition) (Part, error) {
		c.owners.cluster.deliveryReview, c.owners.cluster.deliveryKube = client, c.owners.cluster.Kube
		return build(ctx, c)
	}
}

func composedBrokerRequest(t *testing.T, f *composedFixture, remote string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), "POST", secretstore.BrokerPath, strings.NewReader(`{"path":"registry/value"}`))
	r.RemoteAddr = remote
	r.Header.Set("X-Vault-Token", "invalid")
	w := httptest.NewRecorder()
	f.owners.c.source.ServeHTTP(w, r)
	return w
}
