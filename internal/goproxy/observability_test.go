package goproxy

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyFailureClasses(t *testing.T) {
	for _, test := range []struct {
		name, method, path, backend, class string
		status                             int
	}{
		{"public miss", "GET", "/public.example.test/module/@v/list", "", "", 404},
		{"method", "POST", "/go.example.test/issue764-fixture/@v/list", "", "method_not_allowed", 405},
		{"private miss", "GET", "/go.example.test/unknown/@v/list?token=secret-canary", "", "private_unavailable", 403},
		{"backend tags", "GET", "/go.example.test/issue764-fixture/@v/list", "list", "private_backend_unavailable", 502},
		{"backend info", "GET", "/go.example.test/issue764-fixture/@v/v1.0.0.info", "info", "private_backend_unavailable", 502},
		{"backend mod", "GET", "/go.example.test/issue764-fixture/@v/v1.0.0.mod", "mod", "private_backend_unavailable", 502},
		{"backend zip", "GET", "/go.example.test/issue764-fixture/@v/v1.0.0.zip", "zip", "private_backend_unavailable", 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			h := routingFixture(test.backend).WithLogger(log.New(&output, "", 0))
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.status || response.Header().Get("X-Oberth-GoProxy-Error") != test.class {
				t.Fatalf("status=%d class=%q", response.Code, response.Header().Get("X-Oberth-GoProxy-Error"))
			}
			if test.class == "" {
				if output.Len() != 0 {
					t.Fatalf("normal public miss logged as failure: %s", &output)
				}
			} else if strings.Count(output.String(), "goproxy request failed:") != 1 || !strings.Contains(output.String(), "class="+test.class) {
				t.Fatalf("missing attributable failure: %s", &output)
			}
			if strings.Contains(output.String(), "secret-canary") || strings.Contains(output.String(), "fixture list failure") {
				t.Fatalf("query or raw backend error logged: %s", &output)
			}
		})
	}
}

type failedResponse struct{ header http.Header }

func (w failedResponse) Header() http.Header       { return w.header }
func (w failedResponse) WriteHeader(int)           {}
func (w failedResponse) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestProxyResponseWriteFailureIsAttributable(t *testing.T) {
	var output bytes.Buffer
	h := routingFixture("").WithLogger(log.New(&output, "", 0))
	h.ServeHTTP(failedResponse{header: make(http.Header)}, httptest.NewRequest("GET", "/go.example.test/issue764-fixture/@v/v1.0.0.mod", nil))
	if !strings.Contains(output.String(), "class=response_write_failed status=200") {
		t.Fatal(output.String())
	}
	w := &observedResponse{ResponseWriter: failedResponse{header: make(http.Header)}}
	if _, err := w.Write([]byte("test")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

func TestProxyFailureLogBoundsRequestText(t *testing.T) {
	var output bytes.Buffer
	h := routingFixture("").WithLogger(log.New(&output, "", 0))
	request := httptest.NewRequest(strings.Repeat("X", 4096), "/"+strings.Repeat("x", 4096)+"?secret=canary", nil)
	h.ServeHTTP(httptest.NewRecorder(), request)
	if output.Len() > 512 || strings.Count(output.String(), "\n") != 1 || strings.Contains(output.String(), "canary") {
		t.Fatalf("request text escaped diagnostic bounds: %d bytes", output.Len())
	}
}
