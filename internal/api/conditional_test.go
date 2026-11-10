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
	if summary.Code != http.StatusOK || strings.Contains(body, "run-1") || strings.Contains(body, "atest") || !strings.Contains(body, `"name":"app"`) || !strings.HasPrefix(body, "[") {
		t.Fatalf("summary view: %d %s", summary.Code, body)
	}
}

// The Go-field-names opt-in changes the body, so it must change the ETag:
// a validator from one shape never revalidates the other.
func TestConditionalPollsKeepTheTwoShapesApart(t *testing.T) {
	server, backend := testServer(t)
	server.views = &collectionViews{fakeBackend: backend, repos: []repoSummaryRow{{ID: 1, Name: "app"}}}
	snake := getAPI(t, server, "/api/repos", nil)
	goNames := getAPI(t, server, "/api/repos", map[string]string{GoFieldNamesHeader: "1"})
	if snake.Header().Get("ETag") == "" || snake.Header().Get("ETag") == goNames.Header().Get("ETag") {
		t.Fatalf("shapes share a validator: %q %q", snake.Header().Get("ETag"), goNames.Header().Get("ETag"))
	}
	if !strings.Contains(goNames.Body.String(), `"Name":"app"`) || strings.Contains(snake.Body.String(), `"Name"`) {
		t.Fatalf("shapes: snake=%s go=%s", snake.Body, goNames.Body)
	}
	crossed := getAPI(t, server, "/api/repos", map[string]string{GoFieldNamesHeader: "1", "If-None-Match": snake.Header().Get("ETag")})
	if crossed.Code != http.StatusOK {
		t.Fatalf("a snake_case validator revalidated the Go-names shape: %d", crossed.Code)
	}
	same := getAPI(t, server, "/api/repos", map[string]string{GoFieldNamesHeader: "1", "If-None-Match": goNames.Header().Get("ETag")})
	if same.Code != http.StatusNotModified {
		t.Fatalf("Go-names poll: %d", same.Code)
	}
	if !strings.Contains(strings.Join(snake.Header().Values("Vary"), ","), GoFieldNamesHeader) {
		t.Fatalf("Vary omits the opt-in header: %v", snake.Header().Values("Vary"))
	}
	summary := getAPI(t, server, "/api/repos?view=summary", map[string]string{GoFieldNamesHeader: "1"})
	if !strings.Contains(summary.Body.String(), `"Name":"app"`) {
		t.Fatalf("summary ignored the opt-in: %s", summary.Body)
	}
}
