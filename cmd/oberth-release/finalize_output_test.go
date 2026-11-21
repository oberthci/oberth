package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFinalizeReportsTheLatestDestinationAndVersion(t *testing.T) {
	for _, tc := range []struct{ current, version, suffix string }{
		{"", "v2.0.0", "is v2.0.0."},
		{"v2.0.0", "v2.0.0", "is v2.0.0."},
		{"v3.0.0", "v3.0.0", "stays v3.0.0; v2.0.0 was not made latest."},
	} {
		t.Run(tc.suffix, func(t *testing.T) {
			p, store := storeFixture(t)
			commit := strings.Repeat("a", 40)
			if tc.current != "" {
				store.files["latest"] = marshalLatest(t, latest{Version: tc.current, Commit: commit})
			}
			var output bytes.Buffer
			if err := p.finish(t.Context(), latest{Version: "v2.0.0", Commit: commit}, &output); err != nil {
				t.Fatal("successful finalization returned failure", err)
			}
			if output.String() != "Latest at "+p.origin+"/latest "+tc.suffix+"\n" {
				t.Fatal("closing line is missing or ambiguous", output.String())
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			var got latest
			if err := json.Unmarshal(store.files["latest"], &got); err != nil || got.Version != tc.version {
				t.Fatal("closing line differs from latest", got, err)
			}
			if tc.current != "" && store.revision != 1 {
				t.Fatal("equal or newer latest was rewritten")
			}
		})
	}
}

func TestFinalizeCreatesLatestAfterForbiddenReadWithoutOverwriting(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "hidden existing object"}[exists], func(t *testing.T) { checkForbiddenLatest(t, exists) })
	}
}
func checkForbiddenLatest(t *testing.T, exists bool) {
	t.Helper()

	p, store := storeFixture(t)
	old := []byte("existing latest")
	if exists {
		store.files["latest"] = old
	}
	server := forbiddenLatestServer(t, store, exists)
	defer server.Close()
	p.origin, p.client = server.URL, server.Client()
	var output bytes.Buffer
	err := p.finish(t.Context(), latest{Version: "v1.0.0", Commit: strings.Repeat("a", 40)}, &output)
	store.mu.Lock()
	defer store.mu.Unlock()
	if exists {
		if !errors.Is(err, latestBusy) || !bytes.Equal(store.files["latest"], old) || output.Len() != 0 {
			t.Fatal("forbidden read replaced an existing latest", err, output.String())
		}
		return
	}
	if err != nil || output.String() != "Latest at "+p.origin+"/latest is v1.0.0.\n" {
		t.Fatal("first finalization failed", err, output.String())
	}
	if store.revision != 2 || !bytes.Contains(store.files["latest"], []byte("v1.0.0")) {
		t.Fatal("latest was not created exactly once")
	}
}

func forbiddenLatestServer(t *testing.T, store *objectStore, exists bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" {
			t.Error("latest creation was not conditional")
		}
		if exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		store.serve(w, r)
	}))
}
