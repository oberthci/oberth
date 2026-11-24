package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipReplyReuseDoesNotMixResponses(t *testing.T) {
	for i := range 16 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			s := &Server{}
			for j := range 3 {
				value := fmt.Sprintf("response-%d-%d:%s", i, j, strings.Repeat("x", 2000-j*500))
				checkGzipReply(t, s, value)
			}
		})
	}
}

func checkGzipReply(t *testing.T, s *Server, value string) {
	t.Helper()
	r := httptest.NewRequest("GET", "/data", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	s.writeJSON(w, r, jsonReply{200, value, 4096})
	z, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(value)
	if !bytes.Equal(body, want) || w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("response bytes crossed request boundaries")
	}
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	cached := httptest.NewRecorder()
	s.writeJSON(cached, r, jsonReply{200, value, 4096})
	if cached.Code != 304 || cached.Body.Len() != 0 {
		t.Fatal("gzip reuse changed conditional replies", cached.Code)
	}
}

func TestGzipReplyReusesCompressionState(t *testing.T) {
	data := bytes.Repeat([]byte("response data "), 200)
	allocs := testing.AllocsPerRun(1000, func() { _ = gzipJSON(data) })
	t.Log("allocations per warm gzip response", allocs)
	if allocs > 10 {
		t.Fatal("response allocates fresh compression state", allocs)
	}
}

func BenchmarkGzipJSONReply(b *testing.B) {
	s := &Server{}
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	value := map[string]string{"runs": strings.Repeat("run metadata ", 300)}
	b.ReportAllocs()
	for b.Loop() {
		s.writeJSON(httptest.NewRecorder(), r, jsonReply{200, value, 8192})
	}
}
