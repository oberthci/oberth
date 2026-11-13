package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

type papercutAuth struct{}

func (papercutAuth) Authenticate(_ context.Context, token string) (api.Actor, error) {
	return api.Actor{Identity: "operator", Admin: token == "admin"}, nil
}

type papercutFixture struct {
	path   string
	db     *store.Store
	repo   model.Repository
	logs   *runlog.Store
	server *api.Server
}

func newPapercutFixture(t *testing.T) papercutFixture {
	t.Helper()
	root := t.TempDir()
	db, err := storetest.Open(t.Context(), filepath.Join(root, "state.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	up, err := storetest.CreateUpstream(db, t.Context(), model.UpstreamSpec{Name: "origin", Kind: "forgejo", BaseURL: "https://forge.example.test/team"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "project", UpstreamID: up.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	control, err := service.NewAPI(service.APIConfig{Runs: db, History: db, Repositories: db, Promotions: db, PromotionRuns: db, Logs: logs, MaximumWait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Shutdown(context.Background()) })
	server, err := api.New(papercutAuth{}, control, control, "test", api.WithErrorClassifier(classifyViewError))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewTLSServer(server.Handler())
	t.Cleanup(httpServer.Close)
	t.Cleanup(withTestMCPClient(t, httpServer))
	t.Setenv("OBERTH_MCP_URL", httpServer.URL+"/mcp")
	t.Setenv("OBERTH_TOKEN", "admin")
	return papercutFixture{path: filepath.Join(root, "state.sqlite"), db: db, repo: repo, logs: logs, server: server}
}
func (f papercutFixture) run(t *testing.T, ref string) model.Run {
	t.Helper()
	run, err := storetest.EnqueueRun(f.db, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: strings.Repeat("a", 40), Actor: "operator", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	return run.Run
}
func (f papercutFixture) rest(path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer admin")
	reply := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(reply, request)
	return reply
}
func papercutCall(t *testing.T, tool string, args any) (string, error) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runMCPCall(t.Context(), []string{tool, string(encoded)}, &output)
	return output.String(), err
}

func TestAdminPapercutRunListCLIAndREST(t *testing.T) {
	f := newPapercutFixture(t)
	queued := f.run(t, "feature/queue")
	finished := f.run(t, "feature/finished")
	if _, err := f.db.FinishRun(t.Context(), finished.ID, model.RunResult{Status: model.RunFailed}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []any{"queued", []string{"queued"}} {
		output, err := papercutCall(t, "run_list", map[string]any{"status": status, "repo": f.repo.Name, "ref": queued.Ref, "limit": 1})
		if err != nil || !strings.Contains(output, queued.ID) || strings.Contains(output, finished.ID) {
			t.Fatalf("list CLI=%s %v", output, err)
		}
	}
	for _, view := range []string{"all", "history"} {
		reply := f.rest("/api/runs?status=queued&repo=project&ref=feature%2Fqueue&view=" + view)
		var page struct {
			Runs []model.Run `json:"runs"`
		}
		if err := json.Unmarshal(reply.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		runs := page.Runs
		want := 1
		if view == "history" {
			want = 0
		}
		if reply.Code != http.StatusOK || len(runs) != want {
			t.Fatalf("list REST %s=%d %s", view, reply.Code, reply.Body)
		}
	}
	reply := f.rest("/api/runs?status=queued&status=failed&limit=1")
	if reply.Code != http.StatusOK || !strings.Contains(reply.Body.String(), queued.ID) || !strings.Contains(reply.Body.String(), finished.ID) || reply.Header().Get("X-Oberth-Next-Before") == "" {
		t.Fatalf("mixed REST page=%d %s headers=%v", reply.Code, reply.Body, reply.Header())
	}
	for _, status := range []any{"interrupted", "unknown", []string{}, []any{"queued", 7}} {
		if _, err := papercutCall(t, "run_list", map[string]any{"status": status}); err == nil {
			t.Fatalf("invalid CLI status accepted: %v", status)
		}
	}
	if reply := f.rest("/api/runs?status=unknown"); reply.Code != http.StatusBadRequest {
		t.Fatalf("bad REST status=%d %s", reply.Code, reply.Body)
	}
}

func TestAdminPapercutStepOnlyLogsCLIAndREST(t *testing.T) {
	f := newPapercutFixture(t)
	run := f.run(t, "feature/logs")
	for i, step := range []model.StepResult{{Stage: "build", Step: "compile"}, {Stage: "unit", Step: "check"}, {Stage: "integration", Step: "check"}} {
		step.RunID, step.Ordinal, step.Status = run.ID, i, model.StepPassed
		if _, err := storetest.PutHistoricalStepResult(f.db, t.Context(), step); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	file, err := f.logs.Create(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("[build/compile] compiled successfully\n[unit/check] unit output\n[integration/check] integration output\n"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.logs.BuildIndex(run.ID); err != nil {
		t.Fatal(err)
	}
	output, err := papercutCall(t, "run_logs", map[string]any{"id": run.ID[:8], "step": "compile"})
	if err != nil || !strings.Contains(output, "compiled successfully") {
		t.Fatalf("logs CLI=%s %v", output, err)
	}
	reply := f.rest("/api/runs/" + run.ID[:8] + "/logs?step=compile")
	if reply.Code != http.StatusOK || !strings.Contains(reply.Body.String(), "compiled successfully") {
		t.Fatalf("logs REST=%d %s", reply.Code, reply.Body)
	}
	output, err = papercutCall(t, "run_logs", map[string]any{"id": run.ID, "step": "check"})
	if err == nil || !strings.Contains(output, "integration, unit") {
		t.Fatalf("ambiguous logs CLI=%s %v", output, err)
	}
	reply = f.rest("/api/runs/" + run.ID + "/logs?step=check")
	if reply.Code != http.StatusBadRequest || !strings.Contains(reply.Body.String(), "integration, unit") {
		t.Fatalf("ambiguous logs REST=%d %s", reply.Code, reply.Body)
	}
}

func TestAdminPapercutPromotionPrefixCLI(t *testing.T) {
	f := newPapercutFixture(t)
	promotion, err := f.db.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/promote", SourceSHA: strings.Repeat("b", 40), TargetRef: "main", Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.FinishPromotion(t.Context(), promotion.ID, model.PromotionFailed, "", "fixture failure"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{promotion.ID[:8], strings.ToUpper(promotion.ID[:13]), promotion.ID} {
		output, err := papercutCall(t, "promote_status", map[string]any{"id": id})
		if err != nil || !strings.Contains(output, promotion.ID) {
			t.Fatalf("promotion CLI %s=%s %v", id, output, err)
		}
	}
	// Long polling must keep the resolved full ID after its first observation.
	pending, err := f.db.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/pending", SourceSHA: strings.Repeat("c", 40), TargetRef: "main", Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := papercutCall(t, "promote_status", map[string]any{"id": pending.ID[:8]})
	if err != nil || !strings.Contains(output, pending.ID) || !strings.Contains(output, "still_running") {
		t.Fatalf("pending promotion CLI=%s %v", output, err)
	}
	raw, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ids := []string{"feedface" + strings.Repeat("a", 24), "feedface" + strings.Repeat("b", 24)}
	for _, id := range ids {
		_, err = raw.Exec(`INSERT INTO promotions(id,repo_id,source_branch,source_sha,target_ref,previous_sha,result_sha,actor,status,created_at,updated_at) SELECT ?,repo_id,source_branch,source_sha,target_ref,previous_sha,result_sha,actor,'pending',created_at,updated_at FROM promotions WHERE id=?`, id, pending.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	output, err = papercutCall(t, "promote_status", map[string]any{"id": "feedface"})
	if err == nil || !strings.Contains(output, "ambiguous") || !strings.Contains(output, ids[0]) || !strings.Contains(output, ids[1]) {
		t.Fatalf("ambiguous promotion CLI=%s %v", output, err)
	}

	for _, id := range []string{pending.ID[:7], "zzzzzzzz"} {
		if _, err := papercutCall(t, "promote_status", map[string]any{"id": id}); err == nil {
			t.Fatal(fmt.Sprint("invalid prefix accepted: ", id))
		}
	}
}
