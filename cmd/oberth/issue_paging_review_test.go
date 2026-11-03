package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	if err := runIssues(t.Context(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(out.String(), "older") {
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
	raw, err := collectIssuePages(t.Context(), func(_ context.Context, before int64) (json.RawMessage, error) {
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
	if err := runIssues(t.Context(), []string{"--work-status", "active", "--stale-after", "2h", "--active-stale-after", "5m"}, &out); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(out.String(), "active") || !strings.Contains(out.String(), "true") {
		t.Fatalf("requests=%d output=%s", requests, out.String())
	}
}
