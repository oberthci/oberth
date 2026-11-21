//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"crypto/tls"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCompositionR1HTTPViewsAreBound(t *testing.T) {
	for _, order := range []string{"finished", "open"} {
		t.Run(order, func(t *testing.T) { t.Parallel(); checkCompositionViews(t, order == "open") })
	}
}

func checkCompositionViews(t *testing.T, open bool) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	for path, status := range map[string]int{
		"/api/runs/counts": 200,
		"/api/runs/abcdef01000000000000000000000001/containers":                               404,
		"/api/runs/abcdef01000000000000000000000001/containerlog?node_id=node&container=main": 404,
	} {
		f.assertHTTPView(t, f.adminCertificate, path, status)
	}
	f.enrollJoined(t)
	f.bindClient(t)
	f.git.Git(t, f.git.Work, "push", "-o", "oberth.run=1", "oberth", "HEAD:refs/heads/views")
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	f.deliverSource(t, run)
	f.captureViewLog(t, run.ID)
	for path, status := range map[string]int{
		"/api/runs/counts": 200,
		"/api/runs/abcdef01000000000000000000000001/containers":                               404,
		"/api/runs/abcdef01000000000000000000000001/containerlog?node_id=node&container=main": 404,
	} {
		f.assertHTTPView(t, f.joinedCertificate(t), path, status)
	}
	for _, certificate := range []tls.Certificate{f.adminCertificate, f.joinedCertificate(t)} {
		body := f.assertHTTPView(t, certificate, "/api/runs/"+run.ID+"/containers", 200)
		if !strings.Contains(body, "view-node") {
			t.Fatal("container inventory missing", body)
		}
		body = f.assertHTTPView(t, certificate, "/api/runs/"+run.ID+"/containerlog?node_id=view-node&container=main", 200)
		if !strings.Contains(body, "view output") {
			t.Fatal("retained log missing", body)
		}
	}
	finish := f.pendingNativeLog(t, run)
	if !open {
		finish()
	}
	f.finishNative(t, run)
	f.waitLogProjection(t)
	f.assertRunLogCLI(t, run.ID, open, finish)
}
func (f *composedFixture) captureViewLog(t *testing.T, id string) {
	t.Helper()
	run, err := f.owners.c.store.Run(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := f.owners.c.logs.Reserve(t.Context(), id, runlog.ContainerLog{NodeID: "view-node", Step: run.Admission.Plan.Entries[0].Path, PodUID: "view-pod", PodName: "view-pod", Namespace: f.config.PipelineNamespace, Container: "main"})
	if err != nil || reservation == nil {
		t.Fatal("cannot reserve retained log", err)
	}
	if err = reservation.Capture(t.Context(), runlog.CaptureRequest{Source: io.NopCloser(strings.NewReader("view output\n"))}); err != nil {
		t.Fatal(err)
	}
}
func (f *composedFixture) assertHTTPView(t *testing.T, certificate tls.Certificate, path string, status int) string {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), "GET", f.config.Public.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.client(f.config.Listen.HTTPS, certificate).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != status {
		t.Errorf("view %s returned %d, want %d: %s", path, response.StatusCode, status, body)
	}
	return string(body)
}

func (f *composedFixture) assertRunLogCLI(t *testing.T, id string, open bool, finish func()) {
	t.Helper()
	app, out, diagnostic := f.revokeApplication(t)
	app.config.Now, app.config.Wait = nil, nil
	reads := watchLogReads(t, &app, open, finish)
	if code := app.run(t.Context(), []string{"log", id}); code != 0 {
		t.Fatal("CLI retained log read failed", code, diagnostic.String())
	}
	want := 1
	if open {
		want = 2
	}
	if *reads != want {
		t.Fatal("wrong number of log pages for capture order", *reads, want)
	}
	if !strings.HasPrefix(out.String(), "== build ==\nfake logs\n") || strings.Count(out.String(), "fake logs\n") != 1 || strings.Contains(out.String(), "view output") || !strings.Contains(out.String(), "result: passed") {
		t.Fatal("native step output was missing or mixed with an unrelated receipt", out.String())
	}
}
