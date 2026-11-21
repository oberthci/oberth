package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRuntimeRejectsMissingSourceBudgetAndUnknownSettings(t *testing.T) {
	for _, raw := range []string{`{"source_timeout_seconds":0}`, `{"source_timeout_seconds":-1}`, `{"source_timeout_seconds":3601}`, `{"admission_timeout_seconds":0}`, `{"source_client_burst":2}`, `{"git_binary":"git"}`, `{"future_setting":true}`, `{"default_branch":"main"}`, `{"executor_account":"default"}`} {
		if !strings.Contains(raw, "git_binary") {
			raw = `{"git_binary":"/usr/bin/git",` + raw[1:]
		}
		if _, err := decodeRuntime([]byte(raw)); err == nil {
			t.Fatal("invalid runtime accepted", raw)
		}
	}
	r, err := decodeRuntime(validRuntime())
	if err != nil {
		t.Fatal(err)
	}
	o := serverOwners{settings: r}
	if o.sourceTimeout() != time.Minute {
		t.Fatal("production source binding has no positive deadline")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	r, err = decodeRuntime(raw)
	if err != nil || len(r.Defaults.WorkflowDefaults) != 0 {
		t.Fatal("absent workflow defaults became a null patch", err)
	}
}

func TestRuntimeIssueStaleWindowsAreBoundedInstallationDefaults(t *testing.T) {
	r, err := decodeRuntime(validRuntime())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.issueWindows(); got.General != 24*time.Hour || got.Active != 2*time.Hour {
		t.Fatal("issue stale windows do not reach the service", got)
	}
	for field, values := range map[string][]int{"issue_stale_after_seconds": {0, 59, 31536001}, "issue_active_stale_after_seconds": {-1, 59, 31536001}} {
		for _, value := range values {
			var fields map[string]any
			if err := json.Unmarshal(validRuntime(), &fields); err != nil {
				t.Fatal(err)
			}
			fields[field] = value
			raw, _ := json.Marshal(fields)
			if _, err := decodeRuntime(raw); err == nil || !strings.Contains(err.Error(), "runtime."+field) {
				t.Fatal("out-of-range stale window accepted or unnamed", field, value, err)
			}
		}
	}
}

func TestRuntimeRequiresExplicitGitExecutable(t *testing.T) {
	for _, raw := range []string{`{}`, `{"git_binary":""}`, `{"git_binary":"git"}`} {
		if _, err := decodeRuntime([]byte(raw)); err == nil || !strings.Contains(err.Error(), "runtime.git_binary") {
			t.Fatal("missing or relative Git executable was accepted or unnamed", raw, err)
		}
	}
}
