package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContainerLogCLIUsesNativeSelectors(t *testing.T) {
	body := "[command/output] plain\ntime=2026-01-01 level=info msg=done argo=true\n"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing auth")
		}
		if strings.HasSuffix(request.URL.Path, "/containers") {
			_ = json.NewEncoder(writer).Encode(map[string]any{"run_id": "run-abc", "containers": []any{map[string]any{"node_id": "node", "container": "main", "stored": true}}})
			return
		}
		if request.URL.Path != "/api/runs/run-abc/containerlog" || request.URL.Query().Get("node_id") != "node" || request.URL.Query().Get("container") != "main" || request.URL.Query().Get("limit") != "10" || request.URL.Query().Has("burn") || request.URL.Query().Has("step") {
			t.Error("wrong native request", request.URL)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"run_id": "run-abc", "node_id": "node", "container": "main", "output": body})
	}))
	defer server.Close()
	configure(t, server)
	var output bytes.Buffer
	if err := runRemoteLog(t.Context(), []string{"run-abc", "--node-id", "node", "--container", "main", "--limit", "10"}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != body {
		t.Fatal("native bytes were formatted as old prefixed logs", output.String())
	}
	output.Reset()
	if err := runRemoteLog(t.Context(), []string{"run-abc", "--containers"}, &output); err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Containers []struct {
			NodeID    string `json:"node_id"`
			Container string `json:"container"`
		}
	}
	if err := json.Unmarshal(output.Bytes(), &listing); err != nil || len(listing.Containers) != 1 || listing.Containers[0].NodeID != "node" || requests != 2 {
		t.Fatal(output.String(), err, requests)
	}
}
