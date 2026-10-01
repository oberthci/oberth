package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func artifactRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	return request
}

func TestStoredHTMLIsNeverRenderedOnTheDashboardOrigin(t *testing.T) {
	server, backend := testServer(t)
	backend.artifactBody = `<script>fetch('/api/runs')</script>`

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		artifactRequest(t, "/api/runs/run-abc/artifacts/coverage/index.html"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	header := recorder.Header()
	if got := header.Get("Content-Type"); strings.Contains(got, "text/html") {
		t.Fatalf("Content-Type %q renders repository-authored HTML in the browser", got)
	}
	if got := header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Fatalf("Content-Disposition %q does not force a download", got)
	}
	if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options %q lets the browser sniff its way back to HTML", got)
	}
	if got := header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("Content-Security-Policy %q does not stop the document loading anything", got)
	}
	if recorder.Body.String() != backend.artifactBody {
		t.Fatal("the artifact body was altered in transit")
	}
}

func TestArtifactDownloadNamesTheFileWithoutItsPath(t *testing.T) {
	server, backend := testServer(t)
	backend.artifactBody = "report"

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		artifactRequest(t, "/api/runs/run-abc/artifacts/surefire/TEST-Report.xml"))

	if got := recorder.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="TEST-Report.xml"`) {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if backend.artifactName != "surefire/TEST-Report.xml" {
		t.Fatalf("the handler passed %q to the service, losing the path", backend.artifactName)
	}
}

func TestArtifactEndpointsRequireAuthentication(t *testing.T) {
	server, _ := testServer(t)
	for _, target := range []string{
		"/api/runs/run-abc/artifacts",
		"/api/runs/run-abc/artifacts/report.xml",
	} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s returned %d without a token", target, recorder.Code)
		}
	}
}

func TestArtifactListingIsJSON(t *testing.T) {
	server, _ := testServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, artifactRequest(t, "/api/runs/run-abc/artifacts"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}
	if !strings.Contains(recorder.Body.String(), "surefire/TEST.xml") {
		t.Fatalf("listing did not name the artifact: %s", recorder.Body.String())
	}
}
