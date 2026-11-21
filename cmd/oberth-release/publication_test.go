package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/releaseverify"
)

type objectStore struct {
	mu             sync.Mutex
	files          map[string][]byte
	revision       int
	race           *latest
	reads, deletes int
	t              *testing.T
}

func (s *objectStore) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer fixture" {
		s.t.Error("missing delegated authorization")
		w.WriteHeader(401)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Error(err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.reads++
		value, ok := s.files[name]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if name == "latest" {
			w.Header().Set("ETag", fmt.Sprintf(`"%d"`, s.revision))
		}
		if _, err := w.Write(value); err != nil {
			s.t.Error(err)
		}
	case http.MethodPut:
		s.put(w, r, name, body)
	case http.MethodDelete:
		s.deletes++
		w.WriteHeader(405)
	default:
		w.WriteHeader(405)
	}
}
func (s *objectStore) put(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	if name != "latest" {
		if r.Header.Get("If-None-Match") != "*" {
			s.t.Error("version write was not create-only")
			w.WriteHeader(400)
			return
		}
		if _, ok := s.files[name]; ok {
			w.WriteHeader(412)
			return
		}
	} else {
		if s.race != nil {
			s.files[name] = marshalLatest(s.t, *s.race)
			s.race = nil
			s.revision++
			w.WriteHeader(412)
			return
		}
		_, exists := s.files[name]
		if exists && r.Header.Get("If-Match") == "" {
			s.t.Error("latest write was not conditional")
			w.WriteHeader(400)
			return
		}
		if exists && r.Header.Get("If-Match") != fmt.Sprintf(`"%d"`, s.revision) {
			w.WriteHeader(412)
			return
		}
		if !exists && r.Header.Get("If-None-Match") != "*" {
			s.t.Error("initial latest write was not create-only")
			w.WriteHeader(400)
			return
		}
		s.revision++
	}
	s.files[name] = append([]byte(nil), body...)
	w.WriteHeader(201)
}
func marshalLatest(t *testing.T, value latest) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
func storeFixture(t *testing.T) (publication, *objectStore) {
	t.Helper()
	store := &objectStore{files: make(map[string][]byte), revision: 1, t: t}
	server := httptest.NewServer(http.HandlerFunc(store.serve))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "artifact"), []byte("release bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	return publication{origin: server.URL, token: "fixture", dir: dir, client: server.Client(), wait: func(context.Context, time.Duration) error { return nil }}, store
}

// Covers: REQ-C13-013
func TestMonotonicReleaseFinalizationContract(t *testing.T) {
	t.Run("immutable version objects", immutableObjects)
	for _, tc := range []latestCase{
		{"first release", "", "v1.0.0", "", "v1.0.0"},
		{"newer", "v1.0.0", "v2.0.0", "", "v2.0.0"},
		{"older", "v3.0.0", "v2.0.0", "", "v3.0.0"},
		{"equal", "v2.0.0", "v2.0.0", "", "v2.0.0"},
		{"reread newer on 412", "v1.0.0", "v2.0.0", "v3.0.0", "v3.0.0"},
		{"retry on 412", "v1.0.0", "v2.0.0", "v1.1.0", "v2.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkLatest(t, tc) })
	}
}
func TestEqualVersionCannotChangeCommit(t *testing.T) {
	p, store := storeFixture(t)
	store.files["latest"] = marshalLatest(t, latest{Version: "v1.0.0", Commit: strings.Repeat("a", 40)})
	if _, err := p.advance(t.Context(), latest{Version: "v1.0.0", Commit: strings.Repeat("b", 40)}); err == nil {
		t.Fatal("equal version changed commit")
	}
}
func TestFinalizationRequiresExactCleanReceipt(t *testing.T) {
	o := options{dir: t.TempDir(), receipt: filepath.Join(t.TempDir(), "verified.json")}
	if err := os.WriteFile(filepath.Join(o.dir, "SHA256SUMS"), []byte("list"), 0644); err != nil {
		t.Fatal(err)
	}
	record := releaseverify.Record{Version: "v1.0.0", Commit: strings.Repeat("a", 40)}
	if err := verified(t.Context(), o, record); err == nil {
		t.Fatal("missing receipt accepted")
	}
	if err := writeVerification(t.Context(), o, record); err != nil {
		t.Fatal(err)
	}
	if err := verified(t.Context(), o, record); err != nil {
		t.Fatal(err)
	}
	record.Version = "v0.9.0"
	if err := verified(t.Context(), o, record); err == nil {
		t.Fatal("stale receipt accepted")
	}
	record.Version = "v1.0.0"
	record.Commit = strings.Repeat("b", 40)
	if err := verified(t.Context(), o, record); err == nil {
		t.Fatal("other commit accepted")
	}
	record.Commit = strings.Repeat("a", 40)
	if err := os.WriteFile(filepath.Join(o.dir, "SHA256SUMS"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verified(t.Context(), o, record); err == nil {
		t.Fatal("changed checksum list accepted")
	}
}
func TestPublicationCancellation(t *testing.T) {
	p, _ := storeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.advance(ctx, latest{Version: "v1.0.0", Commit: strings.Repeat("a", 40)}); err == nil {
		t.Fatal("cancelled publication accepted")
	}
}

type latestCase struct{ name, current, want, raced, expected string }

func checkLatest(t *testing.T, tc latestCase) {
	t.Helper()
	p, store := storeFixture(t)
	commit := strings.Repeat("a", 40)
	if tc.current != "" {
		store.files["latest"] = marshalLatest(t, latest{Version: tc.current, Commit: commit})
	}
	if tc.raced != "" {
		store.race = &latest{Version: tc.raced, Commit: commit}
	}
	result, err := p.advance(t.Context(), latest{Version: tc.want, Commit: commit})
	if err != nil || result.Version != tc.expected {
		t.Fatal("latest result differs", result, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	var got latest
	if err := json.Unmarshal(store.files["latest"], &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != tc.expected || store.deletes != 0 {
		t.Fatalf("latest=%s deletes=%d", got.Version, store.deletes)
	}
	if tc.raced != "" && store.reads < 2 {
		t.Fatal("412 was not reread")
	}
}

func immutableObjects(t *testing.T) {
	t.Helper()
	p, store := storeFixture(t)
	if err := p.createFiles(t.Context(), "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := p.createFiles(t.Context(), "v2.0.0"); err != nil {
		t.Fatal("equal rerun", err)
	}
	if err := os.WriteFile(filepath.Join(p.dir, "artifact"), []byte("changed bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := p.createFiles(t.Context(), "v2.0.0"); err == nil {
		t.Fatal("changed version bytes accepted")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if string(store.files["v2.0.0/artifact"]) != "release bytes" || store.deletes != 0 {
		t.Fatal("version bytes were changed or deleted")
	}
}

func TestReleaseStoreRefusals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	p := publication{origin: server.URL, client: server.Client(), dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(p.dir, "artifact"), []byte("bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.createFiles(t.Context(), "v1.0.0"); err == nil {
		t.Fatal("failed write accepted")
	}
	if _, err := p.advance(t.Context(), latest{Version: "v1.0.0"}); err == nil {
		t.Fatal("failed read accepted")
	}
	if _, err := p.compareAndSwap(t.Context(), []byte("bytes"), `"1"`); err == nil {
		t.Fatal("failed CAS accepted")
	}
}
func TestLatestMetadataLimit(t *testing.T) {
	for _, size := range []int{releaseverify.MaxMetadata, releaseverify.MaxMetadata + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			value := marshalLatest(t, latest{Version: "v2.0.0", Commit: strings.Repeat("a", 40)})
			body := append(value, []byte(strings.Repeat(" ", size-len(value)))...)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("ETag", `"1"`)
				if _, err := w.Write(body); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			p := publication{origin: server.URL, client: server.Client()}
			_, err := p.advance(t.Context(), latest{Version: "v1.0.0", Commit: strings.Repeat("a", 40)})
			if (err == nil) != (size == releaseverify.MaxMetadata) {
				t.Fatalf("size %d: %v", size, err)
			}
		})
	}
}

func TestPublicationDoesNotUploadImageInputs(t *testing.T) {
	p, store := storeFixture(t)
	for _, name := range []string{"oberth.oci", "oberth-source-plugin.oci"} {
		if err := os.Mkdir(filepath.Join(p.dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.createFiles(t.Context(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if len(store.files) != 1 || string(store.files["v1.0.0/artifact"]) != "release bytes" {
		t.Fatal("push inputs were published", store.files)
	}
}

type refusedClose struct{ io.Reader }

func (refusedClose) Close() error { return releaseverify.Digest }
func TestStagingReportsVerifierFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staged")
	if err := stageFile(refusedClose{strings.NewReader("candidate bytes")}, path); !errors.Is(err, releaseverify.Digest) {
		t.Fatal("verification failure was lost during staging", err)
	}
}

func TestLatestCASStopsAfterFiveAttemptsWithBackoff(t *testing.T) {
	p, _ := storeFixture(t)
	puts := 0
	var delays []time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(404)
			return
		}
		puts++
		w.WriteHeader(412)
	}))
	defer server.Close()
	p.origin, p.client = server.URL, server.Client()
	p.wait = func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }
	_, err := p.advance(t.Context(), latest{Version: "v1.0.0", Commit: strings.Repeat("a", 40)})
	if !errors.Is(err, latestBusy) || puts != 5 || !reflect.DeepEqual(delays, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}) {
		t.Fatal("unbounded or immediate CAS", puts, delays, err)
	}
}
func TestLatestInvalidHasRepairRemedy(t *testing.T) {
	p, store := storeFixture(t)
	store.files["latest"] = []byte("broken JSON")
	if _, err := p.advance(t.Context(), latest{Version: "v1.0.0"}); !errors.Is(err, latestInvalid) {
		t.Fatal("invalid latest not identified", err)
	}
}
func TestPublicationCommitsManifestLast(t *testing.T) {
	p, _ := storeFixture(t)
	if err := os.Remove(filepath.Join(p.dir, "artifact")); err != nil {
		t.Fatal(err)
	}
	names := append(publishedNames(), "SHA256SUMS", "SHA256SUMS.sigstore.json")
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(p.dir, name), []byte("bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, path.Base(r.URL.Path))
		w.WriteHeader(201)
	}))
	defer server.Close()
	p.origin, p.client = server.URL, server.Client()
	if err := p.createFiles(t.Context(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[len(got)-3:], []string{"release.json", "SHA256SUMS", "SHA256SUMS.sigstore.json"}) {
		t.Fatal("manifest published before artifacts", got)
	}
}
func TestVersionFieldIsExact(t *testing.T) {
	for _, version := range []string{"v1.2.3", "v1.2.30", "prefix-v1.2.3"} {
		if versionMatches([]byte("oberth "+version+" commit=abc date=today\n"), "v1.2.3") != (version == "v1.2.3") {
			t.Fatal("inexact version", version)
		}
	}
}
func TestRetryWaitHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitRetry(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
