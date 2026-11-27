package main

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestAuthorizationCapacityAcceptsNRefusesNPlusOne(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := sourcedelivery.New(sourcedelivery.Config{Now: instant, SourceClientBurst: 6,
		Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) {
			entered <- struct{}{}
			<-release
			return sourcedelivery.Caller{}, &sourcedelivery.Failure{Code: "unauthenticated"}
		},
		Run: func(context.Context, string) (sourcedelivery.Run, error) {
			t.Error("unverified identity reached run read")
			return sourcedelivery.Run{}, nil
		},
		Stage: func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
			t.Error("busy auth reached export")
			return sourcedelivery.Staged{}, nil
		}})
	request := sourcedelivery.LoadRequest{Locator: "run", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = load(t, server, request, "Bearer valid") }()
	}
	<-entered
	<-entered
	finishCapacityProbe(t, capacityProbe{server: server, request: request, entered: entered, release: release, workers: &wg})
}
func TestWorkflowAuthorizationCapacityAcceptsTwoRefusesThird(t *testing.T) {
	f := gittest.New(t)
	b := f.Binding()
	p := prepared(t, b)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := sourcedelivery.New(sourcedelivery.Config{Now: instant, SourceClientBurst: 30,
		Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) {
			return sourcedelivery.Caller{Namespace: "ns", Workflow: "wf", UID: "uid"}, nil
		},
		Run: func(context.Context, string) (sourcedelivery.Run, error) {
			entered <- struct{}{}
			<-release
			return sourcedelivery.Run{Admission: p.View(), Namespace: "ns", Workflow: "wf", UID: "uid"}, nil
		},
		Stage: func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
			return sourcedelivery.Staged{}, &sourcedelivery.Failure{Code: "unavailable", Reason: "export-slot"}
		}})
	request := sourcedelivery.LoadRequest{Locator: "run-1", Descriptor: b.Descriptor}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = load(t, server, request, "Bearer valid") }()
	}
	<-entered
	<-entered
	finishCapacityProbe(t, capacityProbe{server: server, request: request, entered: entered, release: release, workers: &wg})
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenBody) Close() error             { return nil }
func TestPostStreamFailureWithholdsReceiptAndCountsFixedMetric(t *testing.T) {
	f := gittest.New(t)
	b := f.Binding()
	p := prepared(t, b)
	var metrics []string
	server := sourcedelivery.New(sourcedelivery.Config{Now: instant, SourceClientBurst: 30, Metric: func(summary sourcedelivery.Summary) { metrics = append(metrics, summary.Code+"/"+summary.Reason) },
		Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) {
			return sourcedelivery.Caller{Namespace: "ns", Workflow: "wf", UID: "uid"}, nil
		},
		Run: func(context.Context, string) (sourcedelivery.Run, error) {
			return sourcedelivery.Run{Admission: p.View(), Namespace: "ns", Workflow: "wf", UID: "uid"}, nil
		},
		Stage: func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
			return sourcedelivery.Staged{Body: brokenBody{}}, nil
		}})
	w := load(t, server, sourcedelivery.LoadRequest{Locator: "run-1", Descriptor: b.Descriptor}, "Bearer valid")
	if w.Result().Trailer.Get(sourcedelivery.ReceiptTrailer) != "" || len(metrics) != 1 || metrics[0] != "unavailable/other" {
		t.Fatalf("%v %v", w.Header(), metrics)
	}
}
func TestWireReceiptValidatesEveryRequiredField(t *testing.T) {
	request := sourcedelivery.LoadRequest{Locator: "run", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}}
	count := int64(0)
	index := 0
	digest := strings.Repeat("a", 64)
	good := sourcedelivery.LoadReceipt{Version: 1, Locator: request.Locator, Descriptor: request.Descriptor, Stream: sourcedelivery.StreamReceipt{AdmissionDigest: digest, Digest: digest, Bytes: &count, Payload: sourcedelivery.Payload{Kind: sourcecontract.Main, Index: &index}}}
	for _, change := range []func(*sourcedelivery.LoadReceipt){func(r *sourcedelivery.LoadReceipt) { r.Version = 2 }, func(r *sourcedelivery.LoadReceipt) { r.Locator = "another" }, func(r *sourcedelivery.LoadReceipt) { r.Stream.AdmissionDigest = "" }, func(r *sourcedelivery.LoadReceipt) { r.Stream.Bytes = nil }, func(r *sourcedelivery.LoadReceipt) { r.Stream.Payload.Index = nil }, func(r *sourcedelivery.LoadReceipt) { r.Stream.Payload.Kind = sourcecontract.Pinned }} {
		changed := good
		change(&changed)
		value, err := sourcedelivery.EncodeReceipt(changed)
		if err != nil {
			t.Fatal(err)
		}
		if sourcedelivery.VerifyReceipt(value, request, digest) == nil {
			t.Fatal("accepted mutated receipt")
		}
	}
	value, err := sourcedelivery.EncodeReceipt(good)
	if err != nil || sourcedelivery.VerifyReceipt(value, request, digest) != nil {
		t.Fatal(err)
	}
}

func TestActiveTransferCapacityAcceptsTwoWaitsForThird(t *testing.T) {
	f := gittest.New(t)
	b := f.Binding()
	p := prepared(t, b)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := sourcedelivery.New(sourcedelivery.Config{Now: instant, SourceClientBurst: 30,
		Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) {
			return sourcedelivery.Caller{Namespace: "ns", Workflow: "wf", UID: "uid"}, nil
		},
		Run: func(context.Context, string) (sourcedelivery.Run, error) {
			return sourcedelivery.Run{Admission: p.View(), Namespace: "ns", Workflow: "wf", UID: "uid"}, nil
		},
		Stage: func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
			entered <- struct{}{}
			<-release
			return sourcedelivery.Staged{Body: io.NopCloser(strings.NewReader("source")), Bytes: 6}, nil
		}})
	request := sourcedelivery.LoadRequest{Locator: "run-1", Descriptor: b.Descriptor}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = load(t, server, request, "Bearer valid") }()
	}
	<-entered
	<-entered
	finishWaitingTransfer(t, capacityProbe{server: server, request: request, entered: entered, release: release, workers: &wg})
}

func finishWaitingTransfer(t *testing.T, p capacityProbe) {
	t.Helper()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- load(t, p.server, p.request, "Bearer valid") }()
	select {
	case w := <-response:
		t.Errorf("transfer returned before capacity was released: %d", w.Code)
	case <-p.entered:
		t.Error("third transfer exceeded the two active slots")
	case <-time.After(20 * time.Millisecond):
	}
	close(p.release)
	p.workers.Wait()
	select {
	case w := <-response:
		if w.Code != 200 {
			t.Errorf("waiting transfer status %d", w.Code)
		}
	case <-time.After(time.Second):
		t.Error("waiting transfer did not finish after capacity was released")
	}
}

type capacityProbe struct {
	server           *sourcedelivery.Server
	request          sourcedelivery.LoadRequest
	entered, release chan struct{}
	workers          *sync.WaitGroup
}

func finishCapacityProbe(t *testing.T, p capacityProbe) {
	t.Helper()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- load(t, p.server, p.request, "Bearer valid") }()
	admitted := false
	select {
	case w := <-response:
		if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
			t.Errorf("busy status %d: %v", w.Code, w.Header())
		}
	case <-p.entered:
		admitted = true
		t.Error("N+1 request entered a capacity-limited phase")
	}
	close(p.release)
	p.workers.Wait()
	if admitted {
		<-response
	}
}
