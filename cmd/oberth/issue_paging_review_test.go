package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssuesCLIReadsAllPages(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("before") == "1" {
			_, _ = w.Write([]byte(`{"Issues":[{"ID":1,"Title":"older"}],"total":2}`))
		} else {
			_, _ = w.Write([]byte(`{"Issues":[{"ID":2,"Title":"newer"}],"NextBefore":1,"total":2}`))
		}
	}))
	defer server.Close()
	configure(t, server)
	var out bytes.Buffer
	if err := runIssues(t.Context(), []string{"--all"}, &out); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(out.String(), "older") || strings.Contains(out.String(), "more:") {
		t.Fatalf("requests=%d output=%s", requests, out.String())
	}
}

func TestIssuesJSONPreservesIntegerPrecision(t *testing.T) {
	configure(t, remoteServer(t, `{"Issues":[{"ID":9007199254740993,"Title":"exact"}],"total":1}`))
	var out bytes.Buffer
	if err := runIssues(t.Context(), []string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "9007199254740993") {
		t.Fatalf("issue ID lost precision: %s", out.String())
	}
}

func TestCompatibilityIssuePageAliasesStayEqual(t *testing.T) {
	raw, err := collectIssuePages(t.Context(), 0, func(_ context.Context, before int64) (json.RawMessage, error) {
		if before == 0 {
			return json.RawMessage(`{"Issues":[{"id":2}],"issues":[{"id":2}],"NextBefore":1,"next_before":1}`), nil
		}
		return json.RawMessage(`{"Issues":[{"id":1}],"issues":[{"id":1}],"NextBefore":0,"next_before":0}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if string(result["Issues"]) != string(result["issues"]) || string(result["NextBefore"]) != "0" || string(result["next_before"]) != "0" {
		t.Fatalf("aggregated issue aliases disagree: %s", raw)
	}
	var issues []any
	if err = json.Unmarshal(result["issues"], &issues); err != nil || len(issues) != 2 {
		t.Fatal("preferred alias lost older pages", string(raw), err)
	}
}

func TestIssuesCLIWorkspaceFiltersEveryPage(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("work_status") != "active" || r.URL.Query().Get("stale_after") != "2h" || r.URL.Query().Get("active_stale_after") != "5m" {
			t.Errorf("filters lost: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("before") == "1" {
			_, _ = w.Write([]byte(`{"Issues":[{"ID":1,"work_status":"active","stale":true}],"total":2}`))
		} else {
			_, _ = w.Write([]byte(`{"Issues":[{"ID":2,"work_status":"active","stale":false}],"NextBefore":1,"total":2}`))
		}
	}))
	defer server.Close()
	configure(t, server)
	var out bytes.Buffer
	if err := runIssues(t.Context(), []string{"--all", "--work-status", "active", "--stale-after", "2h", "--active-stale-after", "5m"}, &out); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(out.String(), "active") || !strings.Contains(out.String(), "true") {
		t.Fatalf("requests=%d output=%s", requests, out.String())
	}
}

// The default listing reads one page and names the next one; it never walks
// every page of a large history.
func TestIssuesCLIReadsOnePageByDefault(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[{"id":90,"title":"newest","state":"closed"}],"next_before":90,"total":1373}`))
	}))
	defer server.Close()
	configure(t, server)
	var out bytes.Buffer
	if err := runIssues(t.Context(), []string{"--state", "closed"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || !strings.Contains(queries[0], "limit=50") || strings.Contains(queries[0], "before=") {
		t.Fatalf("default listing requests = %q", queries)
	}
	for _, want := range []string{"newest", "1–1 of 1373", "more: oberth issues --state closed --before 90 (or --all)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestIssuesCLIForwardsLimitAndBefore(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"issues":[{"id":7,"title":"older"}],"next_before":0,"total":9}`))
	}))
	defer server.Close()
	configure(t, server)
	var out bytes.Buffer
	if err := runIssues(t.Context(), []string{"--limit", "5", "--before", "8"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "limit=5") || !strings.Contains(query, "before=8") {
		t.Fatalf("query = %q", query)
	}
	if !strings.Contains(out.String(), "1 older than #8, of 9") || strings.Contains(out.String(), "more:") {
		t.Fatalf("output:\n%s", out.String())
	}
	for _, args := range [][]string{{"--limit", "0"}, {"--limit", "51"}, {"--before", "-1"}} {
		if err := runIssues(t.Context(), args, &out); !errors.Is(err, errUsage) {
			t.Fatalf("%v: err = %v, want usage error", args, err)
		}
	}
}
