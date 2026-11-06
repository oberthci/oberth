package api

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type compressionBackend struct{ fakeBackend }

func (*compressionBackend) Repositories(context.Context, Actor) (any, error) {
	return map[string]any{"repositories": []any{map[string]string{"name": strings.Repeat("repository", 300)}}}, nil
}

func TestJSONCompressionRoutes(t *testing.T) {
	backend := &compressionBackend{}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/repos", "/mcp"} {
		var identity string
		for _, encoding := range []string{"", "gzip", "br, gzip;q=0.5", "gzip;q=0", "gzip;q=NaN", "*", "gzip;q=0, gzip"} {
			t.Run(path+"/"+encoding, func(t *testing.T) {
				method, body := http.MethodGet, ""
				if path == "/mcp" {
					method, body = http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
				}
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer valid-token")
				req.Header.Set("Accept-Encoding", encoding)
				recorder := httptest.NewRecorder()
				server.Handler().ServeHTTP(recorder, req)
				if recorder.Code != http.StatusOK || recorder.Header().Get("Vary") != "Accept-Encoding" {
					t.Fatal(recorder.Code, recorder.Header())
				}
				wantGzip := encoding == "gzip" || encoding == "br, gzip;q=0.5"
				var reader io.Reader = recorder.Body
				if wantGzip {
					if recorder.Header().Get("Content-Encoding") != "gzip" || recorder.Header().Get("Content-Length") != "" {
						t.Fatal(recorder.Header())
					}
					gz, err := gzip.NewReader(reader)
					if err != nil {
						t.Fatal(err)
					}
					defer gz.Close()
					reader = gz
				} else if recorder.Header().Get("Content-Encoding") != "" {
					t.Fatal(recorder.Header())
				}
				raw, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				if encoding == "" {
					identity = string(raw)
				} else if string(raw) != identity {
					t.Fatal("compression changed the JSON response")
				}
			})
		}
	}
}

func TestJSONCompressionExclusions(t *testing.T) {
	for _, tc := range []struct {
		name, path, contentType, encoding string
		size                              int
	}{
		{"tiny", "/api/repos", "application/json", "", 100},
		{"artifact JSON", "/api/runs/r/artifacts/report.json", "application/json", "", 4096},
		{"raw log", "/api/runs/r/logs", "text/plain", "", 4096},
		{"already compressed", "/api/repos", "application/json", "gzip", 4096},
		{"outside API", "/healthz", "application/json", "", 4096},
		{"SSE", "/mcp", "text/event-stream", "", 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Repeat("x", tc.size)
			handler := gzipJSON(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.Header().Set("Vary", "Origin, Accept-Encoding")
				_, _ = io.WriteString(w, body)
			}))
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Body.String() != body || rec.Header().Get("Content-Encoding") != tc.encoding {
				t.Fatal("excluded response was changed", rec.Header())
			}
		})
	}
}
