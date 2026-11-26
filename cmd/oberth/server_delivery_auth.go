package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	wfclient "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

// Only the TokenReview runs for an unverified caller, so it alone draws on a
// small capped budget. Reads after it are bound to a reviewed Pod token and
// have their own budget sized for real load: one delivery reads its Pod and
// Workflow once, so 8 steps fetching 4 paths each make 64 reads, which the
// burst and one second serve. Neither budget is the live observer's.
const (
	deliveryReviewQPS, deliveryReviewBurst = 5, 10
	deliveryReadQPS, deliveryReadBurst     = 25, 50
)

func deliveryClients(cfg *rest.Config) (kubernetes.Interface, kubernetes.Interface, wfclient.Interface, error) {
	review, err := kubernetes.NewForConfig(budget(cfg, deliveryReviewQPS, deliveryReviewBurst))
	if err != nil {
		return nil, nil, nil, err
	}
	read := budget(cfg, deliveryReadQPS, deliveryReadBurst)
	kube, err := kubernetes.NewForConfig(read)
	if err != nil {
		return nil, nil, nil, err
	}
	argo, err := wfclient.NewForConfig(read)
	return review, kube, argo, err
}

func budget(cfg *rest.Config, qps float32, burst int) *rest.Config {
	cfg = rest.CopyConfig(cfg)
	cfg.QPS, cfg.Burst = qps, burst
	cfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(qps, burst)
	return cfg
}

func (o *serverOwners) brokerAuthentication() sourcedelivery.KubernetesAuth {
	review, kube := o.cluster.deliveryReview, o.cluster.deliveryKube
	if review == nil || kube == nil {
		review, kube = o.cluster.Kube, o.cluster.Kube
	}
	return sourcedelivery.KubernetesAuth{Audience: o.c.delivery.Access.Audience,
		Review: func(ctx context.Context, r *auth.TokenReview) (*auth.TokenReview, error) {
			return review.AuthenticationV1().TokenReviews().Create(ctx, r, meta.CreateOptions{})
		}, Pod: func(ctx context.Context, ns, name string) (*core.Pod, error) {
			return kube.CoreV1().Pods(ns).Get(ctx, name, meta.GetOptions{})
		}}
}

// Authentication slots are capped in total and held at most once per
// Pod where the watch knows it, otherwise per address. A flooding Pod then holds
// one place across both IP stacks in the review budget's queue,
// and the rest of its requests get a retryable 503 without an API call. An
// address that presented an invalid token is cooled for 30 s, doubling to
// 5 min, until it presents a valid one: it gets the same retryable 503 without
// an API call. The cooldown names the Pod the watch held at that address, so a
// newer Pod given the recycled address is not cooled; an address without a Pod
// is cooled whoever holds it next. The table forgets its oldest address when full.
const (
	deliveryCooldown, deliveryCooldownMax = 30 * time.Second, 5 * time.Minute
	deliveryCooldownAddresses             = 4096
)

type deliveryRefusal struct {
	at, until time.Time
	wait      time.Duration
	pod       string
}

// cools reports whether a refusal recorded for one Pod covers the Pod now at
// the address; an unknown Pod on either side keeps the address cooled.
func (r deliveryRefusal) cools(pod string) bool {
	return r.pod == "" || pod == "" || r.pod == pod
}

type deliveryAuthGate struct {
	mu      sync.Mutex
	limit   int
	now     func() time.Time
	remotes map[string]struct{}
	refused map[string]deliveryRefusal
}

func newAuthGate(limit int, now func() time.Time) *deliveryAuthGate {
	return &deliveryAuthGate{limit: limit, now: now, remotes: map[string]struct{}{}, refused: map[string]deliveryRefusal{}}
}

func (g *deliveryAuthGate) acquire(remote, pod string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r, ok := g.refused[remote]; ok && g.now().Before(r.until) && r.cools(pod) {
		return &sourcedelivery.Failure{Code: "unavailable", Reason: "auth-cap"}
	}
	slot := deliveryAuthSlot(remote, pod)
	if _, busy := g.remotes[slot]; busy || len(g.remotes) >= g.limit {
		return &sourcedelivery.Failure{Code: "unavailable", Reason: "auth-cap"}
	}
	g.remotes[slot] = struct{}{}
	return nil
}

func (g *deliveryAuthGate) release(remote, pod string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.remotes, deliveryAuthSlot(remote, pod))
	var failure *sourcedelivery.Failure
	switch {
	case err == nil:
		delete(g.refused, remote)
	case errors.As(err, &failure) && failure.Code == "unauthenticated":
		r, known := g.refused[remote]
		if !known && len(g.refused) >= deliveryCooldownAddresses {
			g.forgetOldest()
		}
		if !r.cools(pod) {
			r = deliveryRefusal{}
		}
		r.wait = min(max(2*r.wait, deliveryCooldown), deliveryCooldownMax)
		r.at, r.pod = g.now(), pod
		r.until = r.at.Add(r.wait)
		g.refused[remote] = r
	}
}

func deliveryAuthSlot(remote, pod string) string {
	if pod != "" {
		return "pod:" + pod
	}
	return "address:" + remote
}

func (g *deliveryAuthGate) forgetOldest() {
	oldest := ""
	for remote, r := range g.refused {
		if oldest == "" || r.at.Before(g.refused[oldest].at) {
			oldest = remote
		}
	}
	delete(g.refused, oldest)
}

func (o *serverOwners) boundedDeliveryAuthentication(authenticate func(context.Context, string) (sourcedelivery.Caller, error)) func(context.Context, string, string) (sourcedelivery.Caller, error) {
	gate := newAuthGate(max(1, o.settings.SourceClientBurst/3), o.c.clock.Now)
	podAt := o.podAt
	return func(ctx context.Context, remote, jwt string) (caller sourcedelivery.Caller, err error) {
		pod := ""
		if podAt != nil {
			pod = podAt(remote)
		}
		if err := gate.acquire(remote, pod); err != nil {
			return sourcedelivery.Caller{}, err
		}
		defer func() { gate.release(remote, pod, err) }()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		caller, err = authenticate(ctx, jwt)
		if ctx.Err() != nil {
			return caller, &sourcedelivery.Failure{Code: "unavailable", Reason: "auth-cap"}
		}
		return caller, err
	}
}

func remoteHost(r *http.Request) string { return addressHost(r.RemoteAddr) }

func addressHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// A verified caller's identity read fails only transiently unless its Workflow
// is gone or refuses ownership; limiter, deadline and API errors are retryable.
func brokerIdentityFailure(err error) error {
	var failure *sourcedelivery.Failure
	var public *publicerr.Error
	if errors.Is(err, argojob.ErrDeliveryNodePending) {
		return &sourcedelivery.Failure{Code: "unavailable", Reason: "other"}
	}
	if errors.As(err, &failure) || errors.As(err, &public) || apierrors.IsNotFound(err) {
		return err
	}
	return &sourcedelivery.Failure{Code: "unavailable", Reason: "other"}
}

func secretDeliveryRefusal(w http.ResponseWriter, err error) {
	var failure *sourcedelivery.Failure
	if errors.As(err, &failure) && failure.Code == "unavailable" {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(503)
		return
	}
	w.WriteHeader(secretDeliveryStatus(err))
}
