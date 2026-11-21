package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestServeBindsBeforeConstructionAndRunsFromConfig(t *testing.T) {
	f := fixtureServer(t)
	f.beforeCache = func(c *composition) error { return assertBoundAndLive(t, f, c) }
	path := writeServerConfig(t, f.config)
	app := application{server: &f.deps, diagnostic: f.trace}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- app.serve(ctx, []string{"--config=" + path}) }()
	select {
	case <-f.trace.ready:
	case code := <-done:
		t.Fatalf("serve never became ready: %d %s", code, f.trace.String())
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not become ready within fixture budget")
	}
	if readStatus(t, f.client(f.config.Listen.HTTPS), "/readyz") != 200 {
		t.Fatal("server never marked started")
	}
	assertAdministratorFirst(t, f.trace.snapshot())
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve failed: %d %s", code, f.trace.String())
	}
	if f.c.store.Readiness(t.Context()) == nil {
		t.Fatal("store left open")
	}
	if f.clock.ActiveTickers() != 0 {
		t.Fatal("server left tickers running")
	}
}

// Covers: REQ-C11-025
func TestServerShutdownContract(t *testing.T) {
	f := fixtureServer(t)
	requestEntered, release := make(chan struct{}), make(chan struct{})
	f.source = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestEntered)
		<-release
		if r.Context().Err() != nil {
			t.Error("request cancelled before drain completed")
		}
		f.trace.record("request complete")
		w.WriteHeader(204)
	})
	f.closeError = errors.New("service close refused")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := f.start(ctx)
	sshLifetime := <-f.sshContext
	result := make(chan error, 1)
	go func() {
		request, err := http.NewRequestWithContext(t.Context(), "GET", "https://oberth.local/work", nil)
		if err != nil {
			result <- err
			return
		}
		r, err := f.client(f.config.Listen.Source).Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, r.Body)
			err = r.Body.Close()
		}
		result <- err
	}()
	<-requestEntered
	cancel()
	<-f.listener(f.config.Listen.HTTPS).closed
	<-f.listener(f.config.Listen.Source).closed
	assertDrainingParts(sshLifetime, t, f)
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, f.closeError) {
		t.Fatalf("shutdown lost close refusal: %v", err)
	}
	assertStoppedAfterRequest(t, f)
}

func TestServeReportsStartAndSSHFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{{"start", startRefuses}, {"ssh", sshRefuses}, {"close", closeRefuses}} {
		t.Run(tc.name, tc.run)
	}
}
func startRefuses(t *testing.T) {
	f := fixtureServer(t)
	failure := errors.New("start refused")
	f.readyError = failure
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, f.config, f.deps, f.trace) }()
	waitServerFailure(t, done, cancel, failure)
	if f.c.store.Readiness(t.Context()) == nil {
		t.Fatal("failed start left store open")
	}
}
func sshRefuses(t *testing.T) {
	f := fixtureServer(t)
	failure := errors.New("ssh refused")
	exit := make(chan struct{})
	f.sshRun = func(ctx context.Context, _ net.Listener) error {
		select {
		case <-exit:
			return failure
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := f.start(ctx)
	close(exit)
	waitServerFailure(t, done, cancel, failure)
	if f.c.store.Readiness(t.Context()) == nil {
		t.Fatal("SSH failure left store open")
	}
}
func closeRefuses(t *testing.T) {
	f := fixtureServer(t)
	failure := errors.New("close refused")
	f.closeError = failure
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := f.start(ctx)
	cancel()
	waitServerFailure(t, done, cancel, failure)
	if f.c.store.Readiness(t.Context()) == nil {
		t.Fatal("close refusal hid store close")
	}
}
func waitServerFailure(t *testing.T, done <-chan error, cancel context.CancelFunc, want error) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-timer.C:
		t.Error("server did not report its failed part")
		cancel()
		err = <-done
	}
	if !errors.Is(err, want) {
		t.Fatalf("server failure lost: %v", err)
	}
}

func TestLifecycleDetachesSignalAndCancelsReverse(t *testing.T) {
	parent, signal := context.WithCancel(t.Context())
	events := make(chan string, 2)
	parts := []Part{{Name: "first", Run: func(ctx context.Context) error { <-ctx.Done(); events <- "first"; return ctx.Err() }}, {Name: "second", Run: func(ctx context.Context) error { <-ctx.Done(); events <- "second"; return ctx.Err() }}}
	life := newLifecycle(parent, parts)
	if err := life.start(); err != nil {
		t.Fatal(err)
	}
	signal()
	if life.ctx.Err() != nil || life.parts[0].ctx.Err() != nil {
		t.Fatal("signal cancelled part lifetimes")
	}
	if err := life.stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if <-events != "second" || <-events != "first" {
		t.Fatal("part lifetimes cancelled out of reverse order")
	}
}
func TestLifecycleNamesEmptyPartAndChecksBetweenPhases(t *testing.T) {
	empty := newLifecycle(t.Context(), []Part{{Name: ""}})
	if empty.start() == nil {
		t.Fatal("empty part name accepted")
	}
	life := newLifecycle(t.Context(), nil)
	called := false
	life.add(Part{Name: "first", Ready: func(context.Context) error { life.abortStart(); return nil }})
	life.add(Part{Name: "second", Recover: func(context.Context) error { called = true; return nil }})
	if err := life.start(); !errors.Is(err, context.Canceled) || called {
		t.Fatal("work continued after startup cancellation")
	}
}
func TestStuckPartIsNamedAndStoreRemainsOpen(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	storeClosed := make(chan struct{})
	life := newLifecycle(t.Context(), []Part{{Name: "store", Close: func(context.Context) error { close(storeClosed); return nil }}, {Name: "stuck", Run: func(context.Context) error { <-release; return nil }}})
	trace := &serverTrace{ready: make(chan struct{})}
	life.diagnostic = trace
	if err := life.start(); err != nil {
		t.Fatal(err)
	}
	life.timeout = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		expired, cancel := context.WithCancel(ctx)
		cancel()
		return expired, cancel
	}
	if err := life.stop(t.Context()); err == nil {
		t.Fatal("stuck part not reported")
	}
	select {
	case <-storeClosed:
		t.Fatal("store closed beneath live worker")
	default:
	}
	if !strings.Contains(trace.String(), "part stuck") {
		t.Fatal("stuck part unnamed")
	}
}

func TestWatchReadyUsesOwnersCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notify := make(chan func(), 1)
		c := &composition{watch: func(ctx context.Context, ready func()) error { notify <- ready; <-ctx.Done(); return ctx.Err() }}
		part := watchPart(c)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		run := make(chan error, 1)
		go func() { run <- part.Run(ctx) }()
		ready := <-notify
		result := make(chan error, 1)
		go func() { result <- part.Ready(ctx) }()
		synctest.Wait()
		select {
		case <-result:
			t.Fatal("watch became ready before its callback")
		default:
		}
		ready()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		cancel()
		<-run
	})
}

func assertAdministratorFirst(t *testing.T, events []string) {
	t.Helper()
	admin := slices.Index(events, "install administrator")
	count := 0
	for _, event := range events {
		if event == "install administrator" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("administrator installed %d times per start", count)
	}
	if admin < 0 {
		t.Fatal("administrator was not installed")
	}
	for i, event := range events {
		if strings.HasPrefix(event, "recover ") && i < admin {
			t.Fatal("recovery preceded administrator")
		}
	}
}

func assertDrainingParts(sshLifetime context.Context, t *testing.T, f *serverFixture) {
	t.Helper()
	if sshLifetime.Err() != nil {
		t.Error("SSH cancelled before HTTP drain")
	}
	f.mu.Lock()
	for name, ctx := range f.contexts {
		if ctx.Err() != nil {
			t.Errorf("constructor %s cancelled before HTTP drain", name)
		}
	}
	f.mu.Unlock()
	if err := f.c.health.ready(sshLifetime); err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Error("drain did not withdraw readiness")
	}
	response := response{httptest.NewRecorder()}
	f.c.api.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`)))
	if response.Code != 503 {
		t.Error("API did not begin draining")
	}
}

func assertStoppedAfterRequest(t *testing.T, f *serverFixture) {
	t.Helper()
	events := f.trace.snapshot()
	completed := slices.Index(events, "request complete")
	for i, event := range events {
		if strings.HasPrefix(event, "end ") && i < completed {
			t.Fatalf("part ended before HTTP request: %v", events)
		}
	}
	if f.c.store.Readiness(t.Context()) == nil {
		t.Fatal("store left open after owner close refusal")
	}
	for _, name := range []string{"source", "service", "wiring", "native", "cache"} {
		if !slices.Contains(events, "close "+name) {
			t.Fatalf("part %s not closed: %v", name, events)
		}
	}
	if f.clock.ActiveTickers() != 0 {
		t.Fatal("lifetime was not joined")
	}
}

func writeServerConfig(t *testing.T, c serverConfig) string {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "server.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func assertBoundAndLive(t *testing.T, f *serverFixture, c *composition) error {
	t.Helper()
	if f.listener(c.config.Listen.HTTPS) == nil || f.listener(c.config.Listen.Source) == nil {
		return errors.New("HTTP was not bound first")
	}
	client := f.client(c.config.Listen.HTTPS)
	if readStatus(t, client, "/livez") != 200 || readStatus(t, client, "/readyz") != 503 {
		return errors.New("startup probes are not honest")
	}
	return nil
}

func TestUnexpectedStartupRunIsFailureNotInterrupt(t *testing.T) {
	f := fixtureServer(t)
	original := f.deps.constructors.NativeNew
	f.deps.constructors.NativeNew = func(ctx context.Context, c *composition) (Part, error) {
		p, err := original(ctx, c)
		p.Run = func(context.Context) error { return errors.New("native stopped") }
		return p, err
	}
	app := application{server: &f.deps, diagnostic: f.trace}
	if code := app.serve(t.Context(), []string{"--config", writeServerConfig(t, f.config)}); code != 1 {
		t.Fatalf("background failure reported as interrupt: %d", code)
	}
}
