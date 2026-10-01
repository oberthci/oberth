package goproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Faults come from the real handler's cache boundary, not a replacement HTTP
// status shim. Every cache operation has its own independently exercised fault.
type routingCache struct {
	*mockCache
	fail string
}

func (c routingCache) ListTags(ctx context.Context, repo string) ([]string, error) {
	if c.fail == "list" {
		return nil, errors.New("fixture list failure")
	}
	return c.mockCache.ListTags(ctx, repo)
}

func (c routingCache) TagTime(ctx context.Context, repo, tag string) (time.Time, error) {
	if c.fail == "info" {
		return time.Time{}, errors.New("fixture info failure")
	}
	return c.mockCache.TagTime(ctx, repo, tag)
}

func (c routingCache) ShowFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	if c.fail == "mod" {
		return nil, errors.New("fixture mod failure")
	}
	return c.mockCache.ShowFile(ctx, repo, ref, path)
}

func (c routingCache) ArchiveTar(ctx context.Context, repo, ref string, w io.Writer) error {
	if c.fail == "zip" {
		return errors.New("fixture zip failure")
	}
	return c.mockCache.ArchiveTar(ctx, repo, ref, w)
}

func routingFixture(fail string) *Handler {
	return NewHandler(routingCache{mockCache: &mockCache{
		tags: []string{"v1.0.0"},
		files: map[string][]byte{
			"v1.0.0:go.mod": []byte("module go.example.test/issue764-fixture\n\ngo 1.22\n"),
		},
	}, fail: fail}, "go.example.test", map[string]string{"issue764-fixture": "fixture-repository"})
}

func TestPrivateRoutingAllShapes(t *testing.T) {
	for _, module := range []string{"go.example.test", "go.example.test/unknown", "go.example.test/issue764-fixture/nested"} {
		for _, request := range []string{"@latest", "@v/list", "@v/v1.0.0.info", "@v/v1.0.0.mod", "@v/v1.0.0.zip", "@v/unsupported", "invalid"} {
			t.Run(module+"/"+request, func(t *testing.T) {
				assertRoutingStatus(t, routingFixture(""), "/"+module+"/"+request, http.StatusForbidden)
			})
		}
	}
	for _, request := range []string{
		"@v/v1.0.1.info", "@v/v1.0.1.mod", "@v/v1.0.1.zip", "@v/v1.0.info",
		"@v/main.info", "@v/v2.0.0.info", "@v/v1.0.0+metadata.info",
		"@v/v0.0.0-20260929120000-0123456789ab.info", "@v/v1.0.0/other.info",
		"@v/v1.0.0.invalid", "@v/!invalid.info", "@v/", "@v/list?query=1",
		"//../other/@v/list", "@latest/", "@v/../@v/list", "@v/list/extra", "@v/list/@v/list",
	} {
		t.Run(request, func(t *testing.T) {
			assertRoutingStatus(t, routingFixture(""), "/go.example.test/issue764-fixture/"+request, http.StatusForbidden)
		})
	}
	for _, path := range []string{
		"/go.example.test", "//go.example.test/issue764-fixture/@v/list",
		"/go.example.test/../github.com/example/@v/list",
		"/go.example.test/!invalid/@v/list",
	} {
		assertRoutingStatus(t, routingFixture(""), path, http.StatusForbidden)
	}
	for _, path := range []string{"/example.invalid/mod/@latest", "/example.invalid/mod/@v/list", "/go.example.test.example.invalid/mod/@v/list"} {
		assertRoutingStatus(t, routingFixture(""), path, http.StatusNotFound)
	}
}

func TestPrivateRoutingCacheFailures(t *testing.T) {
	for _, request := range []string{"@latest", "@v/list", "@v/v1.0.0.info", "@v/v1.0.0.mod", "@v/v1.0.0.zip"} {
		assertRoutingStatus(t, routingFixture("list"), "/go.example.test/issue764-fixture/"+request, http.StatusBadGateway)
	}
	for _, op := range []string{"info", "mod", "zip"} {
		assertRoutingStatus(t, routingFixture(op), "/go.example.test/issue764-fixture/@v/v1.0.0."+op, http.StatusBadGateway)
	}
	assertRoutingStatus(t, routingFixture("info"), "/go.example.test/issue764-fixture/@latest", http.StatusBadGateway)
}

func TestLatestAndListUseSupportedTags(t *testing.T) {
	for _, tt := range []struct {
		name   string
		tags   []string
		latest string
		list   string
	}{
		{"release preferred", []string{"v1.2.0-rc.1", "v1.0.0", "v1.1.0", "v0.9.0", "v2.0.0", "v1.2", "v0.0.0-20260929120000-0123456789ab", "main"}, "v1.1.0", "v0.9.0\nv1.0.0\nv1.1.0\nv1.2.0-rc.1\n"},
		{"prereleases", []string{"v1.0.0-rc.1", "v1.0.0-beta.1"}, "v1.0.0-rc.1", "v1.0.0-beta.1\nv1.0.0-rc.1\n"},
		{"empty", nil, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(&mockCache{tags: tt.tags}, "go.example.test", map[string]string{"issue764-fixture": "fixture"})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/go.example.test/issue764-fixture/@v/list", nil))
			if rec.Code != http.StatusOK || rec.Body.String() != tt.list {
				t.Fatalf("list: status %d body %q", rec.Code, rec.Body.String())
			}
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/go.example.test/issue764-fixture/@latest", nil))
			if tt.latest == "" {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("empty latest status %d", rec.Code)
				}
			} else if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"Version":"`+tt.latest+`"`) {
				t.Fatalf("latest: status %d body %q", rec.Code, rec.Body.String())
			}
		})
	}
}

func assertRoutingStatus(t *testing.T, handler http.Handler, path string, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != want {
		t.Fatalf("%s status = %d, want %d: %s", path, rec.Code, want, rec.Body.String())
	}
}
