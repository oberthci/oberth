package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

func TestMCPArtifactMissingFileIsNotFoundButIOFailureIsInternal(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(root, "db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	u, err := db.CreateUpstream(t.Context(), model.UpstreamSpec{Name: "origin", Kind: "forgejo", BaseURL: "https://forge.example.test/org"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "repo", UpstreamID: u.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.EnqueueRun(t.Context(), model.RunSpec{RepoID: r.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Trigger: "branch", Actor: "test@host"})
	if err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "artifacts")
	files, err := artifacts.Open(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	control, err := service.NewAPI(service.APIConfig{Runs: db, History: db, Repositories: db, Artifacts: files})
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(toolingAuthenticator{}, control, control, "test", api.WithErrorClassifier(classifyViewError))
	if err != nil {
		t.Fatal(err)
	}
	// A directory in place of a retained file creates a genuine read failure,
	// independent of permissions or whether the test process runs as root.
	if err := os.MkdirAll(filepath.Join(artifactRoot, run.ID, "broken"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, want string }{{"missing", "not found"}, {"broken", "internal error (reference "}} {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"artifact_get","arguments":{"id":%q,"name":%q,"limit":5}}}`, run.ID, tc.name)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer fixture")
		out := httptest.NewRecorder()
		server.Handler().ServeHTTP(out, req)
		var envelope struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if out.Code != http.StatusOK || !envelope.Result.IsError || len(envelope.Result.Content) != 1 {
			t.Fatalf("MCP response: %d %s", out.Code, out.Body)
		}
		message := envelope.Result.Content[0].Text
		if !strings.Contains(message, tc.want) || strings.Contains(message, root) {
			t.Errorf("%s: unsafe or incorrect classification %q", tc.name, message)
		}
	}
}
