package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunPagingUsesServerCursor(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("before") == "opaque-cursor" {
			_, _ = w.Write([]byte(`{"runs":[{"id":"older","status":"passed","queued_at":"2026-01-01T00:00:00Z"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"runs":[{"id":"newer","status":"passed","queued_at":"2026-01-02T00:00:00Z"}],"next_cursor":"opaque-cursor"}`))
		}
	}))
	defer server.Close()
	configure(t, server)
	var out bytes.Buffer
	if err := runRuns(t.Context(), []string{"--all", "--limit", "2"}, &out); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(out.String(), "older") {
		t.Fatalf("requests=%d output=%s", requests, out.String())
	}
}

// `runs --json` keeps the cursor a script needs for the next page, and
// `--all --json` follows every page instead of returning the first one.
func TestRunsJSONCarriesCursorAndAllFollowsPages(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("before") == "41" {
			_, _ = w.Write([]byte(`{"runs":[{"id":"older","status":"passed"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"runs":[{"id":"newer","status":"passed","repo_id":9007199254740993}],"next_before":41}`))
	}))
	defer server.Close()
	configure(t, server)
	var page bytes.Buffer
	if err := runRuns(t.Context(), []string{"--json"}, &page); err != nil {
		t.Fatal(err)
	}
	var one struct {
		Runs       []map[string]any `json:"runs"`
		NextBefore *int64           `json:"next_before"`
	}
	if err := json.Unmarshal(page.Bytes(), &one); err != nil || len(one.Runs) != 1 || one.NextBefore == nil || *one.NextBefore != 41 {
		t.Fatalf("one page = %s (%v)", page.String(), err)
	}
	if !strings.Contains(page.String(), "9007199254740993") {
		t.Fatalf("raw rows lost integer precision: %s", page.String())
	}
	requests = 0
	var all bytes.Buffer
	if err := runRuns(t.Context(), []string{"--all", "--json"}, &all); err != nil {
		t.Fatal(err)
	}
	var every map[string]json.RawMessage
	if err := json.Unmarshal(all.Bytes(), &every); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(string(every["runs"]), "older") || every["next_before"] != nil {
		t.Fatalf("requests=%d --all --json = %s", requests, all.String())
	}
}

func TestRunsJSONScopedCursorIsNextCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"runs":[],"next_cursor":"0123456789abcdef"}`))
	}))
	defer server.Close()
	configure(t, server)
	var out bytes.Buffer
	if err := runRuns(t.Context(), []string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"next_cursor": "0123456789abcdef"`) || !strings.Contains(out.String(), `"runs": []`) {
		t.Fatalf("scoped page = %s", out.String())
	}
}

func TestRunsTableReadsSnakeCaseRows(t *testing.T) {
	configure(t, remoteServer(t, `{"runs":[{"id":"snake-run","status":"passed","ref":"main","queue_sequence":41}],"next_before":41}`))
	var out bytes.Buffer
	if err := runRuns(t.Context(), []string{"--limit", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "snake-run") || !strings.Contains(out.String(), "more: oberth runs --before 41") {
		t.Fatalf("output:\n%s", out.String())
	}
}
