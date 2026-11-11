package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type repoSummaryRow struct {
	ID         int64
	Name       string
	LatestRuns []map[string]any `json:"LatestRuns"`
}

func getAPI(t *testing.T, server *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, request)
	return out
}

// A poll that names the current ETag gets 304 and no body, for identity and
// gzip encodings alike; a changed view gets the new body.
func TestJSONViewsAnswerConditionalPolls(t *testing.T) {
	server, backend := testServer(t)
	views := &collectionViews{fakeBackend: backend, repos: []repoSummaryRow{{ID: 1, Name: "app", LatestRuns: []map[string]any{{"ID": strings.Repeat("r", 2048)}}}}}
	server.views = views
	first := getAPI(t, server, "/api/repos", nil)
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("first poll: %d etag=%q", first.Code, etag)
	}
	for _, encoding := range []string{"", "gzip"} {
		again := getAPI(t, server, "/api/repos", map[string]string{"If-None-Match": etag, "Accept-Encoding": encoding})
		if again.Code != http.StatusNotModified || again.Body.Len() != 0 || again.Header().Get("ETag") != etag || again.Header().Get("Content-Encoding") != "" {
			t.Fatalf("conditional poll (%q): %d body=%d headers=%v", encoding, again.Code, again.Body.Len(), again.Header())
		}
	}
	zipped := getAPI(t, server, "/api/repos", map[string]string{"Accept-Encoding": "gzip"})
	if zipped.Header().Get("ETag") != etag || zipped.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip response validator differs: %v", zipped.Header())
	}
	reader, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(reader); jsonETag(body) != etag {
		t.Fatal("ETag is not the digest of the uncompressed body")
	}
	views.repos = []repoSummaryRow{{ID: 1, Name: "renamed"}}
	changed := getAPI(t, server, "/api/repos", map[string]string{"If-None-Match": etag})
	if changed.Code != http.StatusOK || !strings.Contains(changed.Body.String(), "renamed") || changed.Header().Get("ETag") == etag {
		t.Fatalf("changed view: %d %s", changed.Code, changed.Body)
	}
}

func TestRepositorySummaryOmitsLatestRuns(t *testing.T) {
	server, backend := testServer(t)
	server.views = &collectionViews{fakeBackend: backend, repos: []repoSummaryRow{{ID: 7, Name: "app", LatestRuns: []map[string]any{{"ID": "run-1"}}}}}
	full := getAPI(t, server, "/api/repos", nil)
	summary := getAPI(t, server, "/api/repos?view=summary", nil)
	if !strings.Contains(full.Body.String(), "run-1") {
		t.Fatalf("full view lost its runs: %s", full.Body)
	}
	body := summary.Body.String()
	if summary.Code != http.StatusOK || strings.Contains(body, "run-1") || strings.Contains(body, "atest") || !strings.Contains(body, `"name":"app"`) || !strings.HasPrefix(body, `{"repositories":[`) {
		t.Fatalf("summary view: %d %s", summary.Code, body)
	}
}

// The removed Go-field-names opt-in no longer changes the shape: the header
// and query parameter are ignored and only snake_case names are sent.
func TestGoFieldNamesOptInIsGone(t *testing.T) {
	server, backend := testServer(t)
	server.views = &collectionViews{fakeBackend: backend, repos: []repoSummaryRow{{ID: 1, Name: "app"}}}
	for _, path := range []string{"/api/repos", "/api/repos?go_field_names=1", "/api/repos?view=summary&go_field_names=1"} {
		response := getAPI(t, server, path, map[string]string{"X-Oberth-Go-Field-Names": "1"})
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"Name"`) || !strings.Contains(response.Body.String(), `"name":"app"`) {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body)
		}
		if response.Header().Get("X-Oberth-Go-Field-Names") != "" {
			t.Fatalf("%s echoed the removed opt-in", path)
		}
	}
}
