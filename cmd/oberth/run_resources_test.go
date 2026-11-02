package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/measured"
)

func TestRunResourcesCLIUsesAuthenticatedHistoryAndYAML(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/run-resources" || r.URL.Query().Get("run") != "aaaaaaaa" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Errorf("request %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode(measured.Report{Repository: "repo-01", Steps: []measured.Step{{Burn: "step-12345678", Template: "step-abc12345", Suggested: &measured.Suggested{CPUMilli: 100, MemoryRequestBytes: 64 << 20, MemoryLimitBytes: 128 << 20, ActiveDeadlineSeconds: 120}}}})
	}))
	defer server.Close()
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", "fixture")
	var out strings.Builder
	if e := runRunDetail(t.Context(), []string{"resources", "--run", "aaaaaaaa", "--yaml"}, &out); e != nil {
		t.Fatal(e)
	}
	if calls != 1 || !strings.Contains(out.String(), "paste_ready:") || !strings.Contains(out.String(), "resources:") {
		t.Fatal(out.String())
	}
}

func TestRunResourcesJSONOverridesFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(measured.Report{Repository: "fixture"})
	}))
	defer server.Close()
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", "fixture")
	var out strings.Builder
	if err := runRunResources(t.Context(), []string{"--repo=fixture", "--format=yaml", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(out.String())) {
		t.Fatalf("--json did not select JSON: %s", &out)
	}
}
