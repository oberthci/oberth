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
		_ = json.NewEncoder(w).Encode(measured.Report{Repository: "repo-01", Steps: []measured.Step{{Stage: "step-12345678", Template: "step-abc12345", Suggested: &measured.Suggested{CPUMilli: 100, MemoryRequestBytes: 64 << 20, MemoryLimitBytes: 128 << 20, ActiveDeadlineSeconds: 120}}}})
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

func TestRunResourcesCLIPreservesSizingAcceptance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"steps":[{"template":"compile","sizing_acceptance":{"status":"censored","sufficient":false,"required_runs":5,"comparable_complete_runs":4,"cohorts":1,"censored_attempts":1,"reasons":["memory limit reached"]}}]}`))
	}))
	defer server.Close()
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", "fixture")
	for _, format := range []string{"json", "yaml"} {
		var out strings.Builder
		if err := runRunResources(t.Context(), []string{"--repo=fixture", "--format=" + format}, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "sizing_acceptance") || !strings.Contains(out.String(), "censored") || !strings.Contains(out.String(), "memory limit reached") {
			t.Fatal(out.String())
		}
	}
}

func TestRunResourcesCLIOptsIntoResourceOnlyAdvice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("resource_only") != "true" {
			t.Errorf("CLI did not advertise safe resource-only rendering: %s", r.URL)
			_, _ = w.Write([]byte(`{"steps":[{"suggested":null}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"steps":[{"deadline_no_suggestion":"Pod finalization is not measured","suggested":{"cpu_milli":100,"memory_request_bytes":67108864,"memory_limit_bytes":134217728,"ephemeral_request_bytes":67108864,"ephemeral_limit_bytes":134217728}}]}`))
	}))
	defer server.Close()
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", "fixture")
	for _, format := range []string{"json", "yaml"} {
		var out strings.Builder
		if err := runRunResources(t.Context(), []string{"--repo=fixture", "--format=" + format}, &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "activeDeadlineSeconds") || !strings.Contains(out.String(), "memory_limit_bytes") || !strings.Contains(out.String(), "finalization") || (format == "yaml" && !strings.Contains(out.String(), "paste_ready:")) {
			t.Fatalf("resource-only transport lost advice or invented deadline: %s", &out)
		}
	}
}
