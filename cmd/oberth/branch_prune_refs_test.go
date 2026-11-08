package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBranchPruneRefsCLISendsTheSelection(t *testing.T) {
	const token = "prune-refs-test-token"
	var got map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		body, err := json.Marshal(request.Params)
		if err != nil {
			t.Error(err)
			return
		}
		var params toolsCallParams
		if err := json.Unmarshal(body, &params); err != nil {
			t.Error(err)
			return
		}
		if request.Method != mcpMethodToolsCall || params.Name != "ref_delete_many" {
			t.Errorf("unexpected request: %+v %+v", request, params)
		}
		if err := json.Unmarshal(params.Arguments, &got); err != nil {
			t.Error(err)
		}
		testWriteJSONRPCResult(w, &request.ID, toolsCallResult{Content: []mcpContent{{Type: "text", Text: `{"deleted":1}`}}})
	}))
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	var output strings.Builder
	sha := strings.Repeat("a", 40)
	if err := runBranch(t.Context(), []string{"prune-refs", "codeberg/acme/repo", "--ref", "refs/remotes/upstream/main=" + sha,
		"--allow-unreachable", "--confirm", "--plan-id", "plan-1"}, &output); err != nil || output.String() != "{\"deleted\":1}\n" {
		t.Fatal(output.String(), err)
	}
	encoded, _ := json.Marshal(got)
	want := `{"allow_unreachable":true,"confirm":true,"plan_id":"plan-1","refs":[{"expected_sha":"` + sha + `","ref":"refs/remotes/upstream/main"}],"repo":"codeberg/acme/repo"}`
	if string(encoded) != want {
		t.Fatalf("arguments = %s\nwant        %s", encoded, want)
	}
	got = nil
	output.Reset()
	if err := runBranch(t.Context(), []string{"prune-refs", "codeberg/acme/repo", "--prefix", "refs/remotes/upstream/"}, &output); err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(got); string(encoded) != `{"prefixes":["refs/remotes/upstream/"],"repo":"codeberg/acme/repo"}` {
		t.Fatalf("prefix arguments = %s", encoded)
	}
	if err := runBranch(t.Context(), []string{"prune-refs", "codeberg/acme/repo", "--ref", "refs/remotes/upstream/main"}, &output); !errors.Is(err, errUsage) {
		t.Fatalf("malformed --ref = %v", err)
	}
}
