package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestArtifactDownloadDeclaresLengthAndDigestOfTheRawBytes(t *testing.T) {
	server, backend := testServer(t)
	backend.artifactBody = "\x7fELF\x00\x00\xff\xfe binary\n"

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, artifactRequest(t, "/api/runs/run-abc/artifacts/build/tool"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(len(backend.artifactBody)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(backend.artifactBody))
	}
	sum := sha256.Sum256([]byte(backend.artifactBody))
	if got := recorder.Header().Get(ArtifactSHA256Header); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("%s = %q", ArtifactSHA256Header, got)
	}
	if recorder.Body.String() != backend.artifactBody {
		t.Fatal("the artifact body was altered in transit")
	}
}

// shortArtifacts declares more bytes than its content yields, as a file
// truncated underneath an open download would.
type shortArtifacts struct{ *fakeBackend }

func (backend shortArtifacts) OpenRunArtifact(ctx context.Context, actor Actor, id, name string) (ArtifactObject, error) {
	object, err := backend.fakeBackend.OpenRunArtifact(ctx, actor, id, name)
	object.Size += 1 << 20
	return object, err
}

func TestArtifactDownloadAbortsInsteadOfEndingShort(t *testing.T) {
	backend := &fakeBackend{artifactBody: strings.Repeat("x", 3<<20)}
	server, err := New(backend, backend, shortArtifacts{backend}, "test")
	if err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewServer(server.Handler())
	defer listener.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, listener.URL+"/api/runs/run-abc/artifacts/big.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer valid-token")
	response, err := listener.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err == nil {
		t.Fatalf("a short artifact ended as a clean %d-byte success", len(body))
	}
	if int64(len(body)) >= response.ContentLength {
		t.Fatalf("read %d of declared %d bytes yet the transfer failed", len(body), response.ContentLength)
	}
}

func TestNewRefusesViewsWithoutRawArtifactDownloads(t *testing.T) {
	backend := &fakeBackend{}
	textOnly := struct{ ViewService }{backend}
	if _, err := New(backend, backend, textOnly, "test"); err == nil {
		t.Fatal("a view service without raw downloads would fall back to the bounded text view")
	}
}
