package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResourceMessageDecodeAndRunMessage(t *testing.T) {
	var run remoteRun
	if err := json.Unmarshal([]byte(`{"display_reason":"server restart, retry needed","resource_message":"2 steps have undeclared CPU/memory requests."}`), &run); err != nil {
		t.Fatal(err)
	}
	if run.DisplayReason != "server restart, retry needed" || displayRunReason(run) != "server restart, retry needed" {
		t.Fatal("separate resource message lost or replaced the reason")
	}
}

func TestRunDetailKeepsFullEvidenceBesideConciseReason(t *testing.T) {
	id := strings.Repeat("a", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Run":{"ID":"` + id + `","Status":"failed","Phase":"test","display_reason":"test/unit-tests failed","Error":"full unabridged controller evidence","Reason":"full operator provenance","resource_message":"detail resource advice"},"Steps":[]}`))
	}))
	defer server.Close()
	configure(t, server)
	var out strings.Builder
	if err := runRunDetail(context.Background(), []string{id}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"test/unit-tests failed", "full unabridged controller evidence", "full operator provenance", "detail resource advice"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("detail lost %q: %s", want, out.String())
		}
	}
}
