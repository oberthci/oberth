package goproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnconfiguredProxyCannotReadMutableTags(t *testing.T) {
	h := NewHandler(&mockCache{tags: []string{"v1.0.0"}, files: map[string][]byte{"v1.0.0:go.mod": []byte("module go.example.test/wire\n")}}, "go.example.test", map[string]string{"wire": "fixture"})
	for _, suffix := range []string{"@v/v1.0.0.info", "@v/v1.0.0.mod", "@v/v1.0.0.zip", "@v/list", "@latest"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", "/go.example.test/wire/"+suffix, nil))
		if r.Code != http.StatusServiceUnavailable {
			t.Errorf("uncached %s = %d, want 503", suffix, r.Code)
		}
	}
}
