package main

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/releaseverify"
)

func TestOnlineReleaseAcceptsCompressedAndChunkedOrigins(t *testing.T) {
	_, dir := signedInstallRelease(t, "v0.2.0")
	for _, shape := range []string{"length", "gzip", "chunked", "gzip-over-bound", "chunked-over-bound", "tampered", "redirected", "redirected-tampered"} {

		t.Run(shape, func(t *testing.T) { checkOnlineReleaseShape(t, dir, shape) })
	}
}

func checkOnlineReleaseShape(t *testing.T, dir, shape string) {
	t.Helper()
	origin := standInReleaseOrigin(t, dir, shape)
	defer origin.Close()
	address, _ := url.Parse(origin.URL)
	files := releaseHTTPFiles{ctx: t.Context(), version: "v0.2.0", transport: testTransport(func(r *http.Request) (*http.Response, error) {
		request := r.Clone(r.Context())
		request.URL.Scheme, request.URL.Host = address.Scheme, address.Host
		return origin.Client().Transport.RoundTrip(request)
	})}
	release, err := installer.LoadRelease(t.Context(), files, "v0.2.0")
	switch {
	case strings.Contains(shape, "over-bound"):
		if !errors.Is(err, releaseverify.Limit) {
			t.Fatal("body bound not enforced", err)
		}
	case strings.HasSuffix(shape, "tampered"):
		if !errors.Is(err, releaseverify.Digest) {
			t.Fatal("stream digest not checked", err)
		}
	default:
		if err != nil || !release.Verified() {
			t.Fatal("valid origin refused", err)
		}
	}
}

func standInReleaseOrigin(t *testing.T, dir, shape string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(shape, "redirected") && !strings.HasPrefix(r.URL.Path, "/asset/") {
			http.Redirect(w, r, "https://release-assets.githubusercontent.com/asset/"+path.Base(r.URL.Path), http.StatusFound)
			return
		}
		body, err := os.ReadFile(dir + "/" + path.Base(r.URL.Path))
		if err != nil {
			t.Error(err)
			w.WriteHeader(404)
			return
		}
		if path.Base(r.URL.Path) == "release.json" {
			if strings.Contains(shape, "over-bound") {
				body = make([]byte, releaseverify.MaxMetadata+1)
			}
			if strings.HasSuffix(shape, "tampered") {
				body[0] ^= 1
			}
		}
		if strings.HasPrefix(shape, "gzip") && r.Header.Get("Accept-Encoding") != "gzip" {
			t.Error("transport did not request compression")
		}
		sendReleaseBody(w, body, shape)
	}))
}

func sendReleaseBody(w http.ResponseWriter, body []byte, shape string) {
	// The bounded consumer can close a response without draining it.
	if strings.HasPrefix(shape, "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		_, _ = writer.Write(body)
		_ = writer.Close()
		return
	}
	if strings.HasPrefix(shape, "chunked") {
		w.(http.Flusher).Flush()
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	_, _ = w.Write(body)
}

func TestReleaseAdapterIgnoresDeclaredLength(t *testing.T) {
	for _, size := range []int64{-1, 0, releaseverify.MaxArtifact + 1} {
		files := releaseHTTPFiles{ctx: t.Context(), version: "v0.2.0", transport: testTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, ContentLength: size, Body: io.NopCloser(strings.NewReader("body"))}, nil
		})}
		f, err := files.Open("release.json")
		if err != nil {
			t.Fatal(err)
		}
		info, err := f.Stat()
		if err != nil || info.Size() != -1 {
			t.Fatal("header became a verifier bound", info, err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
