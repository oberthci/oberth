package api

import (
	"compress/gzip"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// gzipJSON compresses the buffered JSON views, never streaming or download
// routes. writeJSON writes its complete marshaled body in one Write, so deciding
// on that write needs no additional response buffer and leaves tiny bodies alone.
func gzipJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (!strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/mcp") ||
			strings.HasSuffix(r.URL.Path, "/stream") || strings.Contains(r.URL.Path, "/artifacts/") ||
			r.Header.Get("Range") != "" || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header) {
			next.ServeHTTP(w, r)
			return
		}
		writer := &jsonGzipWriter{ResponseWriter: w}
		defer writer.finish()
		next.ServeHTTP(writer, r)
	})
}

func acceptsGzip(header http.Header) bool {
	accepted := false
	for _, value := range header.Values("Accept-Encoding") {
		for _, item := range strings.Split(value, ",") {
			encoding, params, err := mime.ParseMediaType(strings.TrimSpace(item))
			if err != nil || !strings.EqualFold(encoding, "gzip") {
				continue
			}
			if raw, ok := params["q"]; ok {
				quality, err := strconv.ParseFloat(raw, 64)
				if err != nil || !(quality > 0 && quality <= 1) {
					return false
				}
			}
			accepted = true
		}
	}
	return accepted
}

type jsonGzipWriter struct {
	http.ResponseWriter
	status     int
	started    bool
	compressor *gzip.Writer
}

func (w *jsonGzipWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *jsonGzipWriter) WriteHeader(status int) {
	if w.started || w.status != 0 {
		return
	}
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
}

func (w *jsonGzipWriter) start(compress bool) {
	if w.started {
		return
	}
	w.started = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if compress {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		w.compressor = gzip.NewWriter(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(w.status)
}

func (w *jsonGzipWriter) Write(body []byte) (int, error) {
	media, _, _ := mime.ParseMediaType(w.Header().Get("Content-Type"))
	w.start(len(body) >= 1024 && media == "application/json" && w.Header().Get("Content-Encoding") == "" && w.status != http.StatusNoContent && w.status != http.StatusNotModified && w.status != http.StatusPartialContent)
	if w.compressor != nil {
		return w.compressor.Write(body)
	}
	return w.ResponseWriter.Write(body)
}

// A flush before the first body write commits identity encoding. SSE and other
// non-JSON streams pass straight through, including their per-event flushes.
func (w *jsonGzipWriter) FlushError() error {
	w.start(false)
	if w.compressor != nil {
		if err := w.compressor.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *jsonGzipWriter) finish() {
	w.start(false)
	if w.compressor != nil {
		_ = w.compressor.Close()
	}
}
