package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// jsonETag is the weak validator writeJSON puts on every 200 JSON view: a
// digest of the uncompressed body, so it is the same for gzip and identity
// encodings. The dashboard sends it back as If-None-Match while polling.
func jsonETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"` + hex.EncodeToString(sum[:12]) + `"`
}

// conditionalJSON answers a GET whose If-None-Match names the ETag the
// handler set with 304 Not Modified and no body. Handlers that set no ETag
// (streams, downloads, MCP) pass through unchanged.
func conditionalJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&conditionalWriter{ResponseWriter: w, request: r}, r)
	})
}

type conditionalWriter struct {
	http.ResponseWriter
	request     *http.Request
	decided     bool
	notModified bool
}

func (w *conditionalWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *conditionalWriter) WriteHeader(status int) {
	if w.decided || status < 200 {
		if status < 200 {
			w.ResponseWriter.WriteHeader(status)
		}
		return
	}
	w.decided = true
	etag := w.Header().Get("ETag")
	if etag != "" && w.request.Method != http.MethodGet {
		w.Header().Del("ETag")
		etag = ""
	}
	if status == http.StatusOK && etag != "" && etagMatches(w.request.Header.Get("If-None-Match"), strings.TrimPrefix(etag, "W/")) {
		w.notModified = true
		w.Header().Del("Content-Type")
		w.Header().Del("Content-Length")
		w.ResponseWriter.WriteHeader(http.StatusNotModified)
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *conditionalWriter) Write(body []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.notModified {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}
