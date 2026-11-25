package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParallelSourceBackpressureThenAuthorFailureOpensCIIssue(t *testing.T) {
	f := newDeliveryFixture(t)
	r := f.run
	logs := f.o.c.logs
	w, p := f.w.DeepCopy(), f.pod.DeepCopy()
	w.Status.Phase = wf.WorkflowFailed
	n := w.Status.Nodes["node"]
	n.Phase = wf.NodeFailed
	n.FinishedAt = meta.NewTime(f.o.c.clock.Now())
	w.Status.Nodes[n.ID] = n
	p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1, FinishedAt: n.FinishedAt}}}}
	identity, err := argojob.DeliveryIdentity(f.w, f.pod)
	if err != nil {
		t.Fatal(err)
	}
	identity.Kind, identity.StartedAt = "source", f.o.c.clock.Now()
	descriptor := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}
	s, entered, release := newBackpressureServer(w, p, logs, identity)
	done := make(chan int, 1)
	body, err := json.Marshal(sourcedelivery.LoadRequest{Locator: r.ID, Descriptor: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	load := func(bearer string) int {
		request := httptest.NewRequestWithContext(t.Context(), "POST", sourcedelivery.LoadPath, strings.NewReader(string(body)))
		request.Header.Set("Authorization", "Bearer "+bearer)
		reply := httptest.NewRecorder()
		s.ServeHTTP(reply, request)
		return reply.Code
	}
	go func() { done <- load("first") }()
	select {
	case <-entered:
	case code := <-done:
		t.Fatal("first source request did not reach staging", code)
	}
	code := load("second")
	close(release)
	if first := <-done; first != 200 || code != 503 {
		t.Fatal("parallel steps did not exercise backpressure", first, code)
	}
	if code = load("second"); code != 200 {
		t.Fatal("source retry failed", code)
	}
	receipts, err := logs.Infrastructure(t.Context(), r.ID)
	if err != nil || len(receipts) != 0 {
		t.Fatal("retryable backpressure minted receipt", receipts, err)
	}
	out, err := argojob.Project(t.Context(), argojob.Projection{RunID: r.ID, Workflow: w, Pods: []*core.Pod{p}, Plan: r.Admission.Plan, Infrastructure: receipts})
	if err != nil {
		t.Fatal(err)
	}
	assertBackpressureCIIssue(t, f, backpressureService(t, f), out)
}

func newBackpressureServer(w *wf.Workflow, p *core.Pod, logs *runlog.Store, base runlog.InfrastructureReceipt) (*sourcedelivery.Server, chan struct{}, chan struct{}) {
	descriptor := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}
	entered, release := make(chan struct{}), make(chan struct{})
	var staging atomic.Bool
	var count atomic.Int32
	c := sourcedelivery.Config{SourceClientBurst: 9, Now: func() time.Time { return base.StartedAt }, Authenticate: func(_ context.Context, bearer string) (sourcedelivery.Caller, error) {
		pod := p.DeepCopy()
		if bearer == "first" {
			pod.UID = "first"
		}
		return sourcedelivery.Caller{Namespace: w.Namespace, Workflow: w.Name, UID: string(w.UID), Pod: pod}, nil
	}, Run: func(context.Context, string) (sourcedelivery.Run, error) {
		return sourcedelivery.Run{Namespace: w.Namespace, Workflow: w.Name, UID: string(w.UID), Admission: admissioncontract.PreparedView{RunID: w.Name, Digest: strings.Repeat("a", 64), SourceBindings: []sourcecontract.Binding{{Descriptor: descriptor}}}}, nil
	}, Stage: func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
		if !staging.CompareAndSwap(false, true) {
			return sourcedelivery.Staged{}, &sourcedelivery.Failure{Code: "unavailable", Reason: "export-slot"}
		}
		defer staging.Store(false)
		if count.Add(1) == 1 {
			close(entered)
			<-release
		}
		return sourcedelivery.Staged{Body: io.NopCloser(strings.NewReader("source")), Bytes: 6}, nil
	}, RecordFailure: func(ctx context.Context, who sourcedelivery.Caller, start time.Time) error {
		r := base
		r.PodUID, r.StartedAt, r.FinishedAt = string(who.Pod.UID), start, base.StartedAt
		return logs.RecordInfrastructure(ctx, r)
	}}
	return sourcedelivery.New(c), entered, release
}

func backpressureService(t *testing.T, f deliveryFixture) *service.Service {
	t.Helper()
	db, absent := f.o.c.store, collectedNativePorts{}
	c := service.Config{Publications: db, Promotions: db, MovingGit: absent, HoldStore: db, Holds: absent, Runs: db, Catalog: db, Events: db, Git: absent, Source: serviceSource{defaultBranch: "refs/heads/main"}, Exec: f.o, Starter: absent, Logs: serverLogs{f.o}, Authority: retainedReadAuthority{}, Now: f.o.c.clock.Now, NewID: func() string { return "unused" }, OnError: func(err error) { t.Error(err) }, AfterFunc: func(d time.Duration, fn func()) service.Timer { return time.AfterFunc(d, fn) }}
	s, err := service.New(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}

func assertBackpressureCIIssue(t *testing.T, f deliveryFixture, s *service.Service, out argojob.Observation) {
	t.Helper()
	if out.Failure == nil || out.Failure.Class != stepmodel.FailureAuthor {
		t.Fatal("backpressure excused author failure", out.Failure)
	}
	if err := s.Finished(t.Context(), service.FinishedRun{RunID: f.run.ID, Status: out.Status, Failure: out.Failure}); err != nil {
		t.Fatal(err)
	}
	page, err := f.o.c.store.ScopedIssues(t.Context(), stateport.IssueScope{Global: true}, model.IssueFilter{Kind: model.IssueCI, Limit: 50})
	if err != nil || len(page.Issues) != 1 || page.Issues[0].CIWorkID != f.run.ID || page.Issues[0].State != model.IssueOpen {
		t.Fatal("author failure lost CI issue", page, err)
	}
}
