package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type latencyObserver struct{ calls atomic.Int32 }

func (o *latencyObserver) ObserveScheduling(ctx context.Context, _ model.Run) (*model.ExecutionObservation, error) {
	o.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

type latencyDemand struct {
	service.ResourceDeclarationReader
	calls atomic.Int32
}

func (o *latencyDemand) RunPodDemand(ctx context.Context, _ service.WorkflowRequest) (*argoworkflow.PodDemand, error) {
	o.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

type apiLatencyFixture struct {
	handler http.Handler
	health  Health
	control *service.API
	probes  *atomic.Int32
	token   string
}

func newAPILatencyFixture(tb testing.TB) apiLatencyFixture {
	tb.Helper()
	ctx := context.Background()
	path := filepath.Join(tb.TempDir(), "history.sqlite")
	database, err := storetest.Open(ctx, path, store.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := database.Close(); err != nil {
			tb.Error(err)
		}
	})
	for u := range 4 {
		upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{Name: fmt.Sprintf("forge-%d", u), Kind: "ssh", BaseURL: fmt.Sprintf("ssh://git@forge-%d.example/org", u)})
		if err != nil {
			tb.Fatal(err)
		}
		for r := range 5 {
			if _, err := database.CreateRepository(ctx, model.RepositorySpec{Name: fmt.Sprintf("repo-%d-%d", u, r), UpstreamID: upstream.ID, DefaultBranch: "main"}); err != nil {
				tb.Fatal(err)
			}
		}
	}
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		tb.Fatal(err)
	}
	defer seed.Close()
	_, err = seed.ExecContext(ctx, `WITH RECURSIVE n(i) AS(VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<50000)
 INSERT INTO runs(id,repo_id,ref_kind,ref,sha,actor,trigger,phase,status,error,queued_at,started_at,finished_at,created_at,updated_at)
 SELECT printf('%032x',i),1+i%20,'branch',printf('feature/ref-%04d',i%2000),?,'fixture','branch','job','failed',?,i,i,i+1,i,i FROM n`, strings.Repeat("a", 40), strings.Repeat("e", 2048))
	if err != nil {
		tb.Fatal(err)
	}
	_, err = seed.ExecContext(ctx, `WITH RECURSIVE n(i) AS(VALUES(50001) UNION ALL SELECT i+1 FROM n WHERE i<50060)
 INSERT INTO runs(id,repo_id,ref_kind,ref,sha,actor,trigger,phase,status,job_name,queued_at,started_at,created_at,updated_at)
 SELECT printf('%032x',i),1+i%20,'branch',printf('feature/active-%d',i),?,'fixture','branch','job',CASE WHEN i<=50050 THEN 'queued' ELSE 'running' END,printf('workflow-%d',i),i,CASE WHEN i>50050 THEN i END,i,i FROM n`, strings.Repeat("b", 40))
	if err != nil {
		tb.Fatal(err)
	}
	_, err = seed.ExecContext(ctx, `INSERT INTO step_results(run_id,burn,step,ordinal,status,exit_code,log_start,log_end,declared_size,started_at,finished_at,recorded_at)
 SELECT id,'build',step,ordinal,'passed',0,0,0,'M',CASE WHEN runs.status='failed' THEN 1 END,CASE WHEN runs.status='failed' THEN 2 END,2
 FROM runs CROSS JOIN(SELECT 'test' AS step,0 AS ordinal UNION ALL SELECT 'check',1) WHERE runs.status='failed'`)
	if err != nil {
		tb.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(tb.TempDir(), "logs"))
	if err != nil {
		tb.Fatal(err)
	}
	for i := 50051; i <= 50060; i++ {
		if err := logs.InitializeStepProgress(fmt.Sprintf("%032x", i)); err != nil {
			tb.Fatal(err)
		}
	}
	var token bytes.Buffer
	issuer := auth.Issuer{Tokens: database, Bind: func(ctx context.Context, c model.TokenCredential) error {
		_, err := database.RegisterUplink(ctx, "fixture", model.UplinkSpec{Identity: "fixture", Fingerprint: "SHA256:fixture", TokenCredentialID: c.ID, Admin: true})
		return err
	}}
	if _, err := issuer.Issue(ctx, "fixture", &token); err != nil {
		tb.Fatal(err)
	}
	authenticator, err := service.NewAuthenticator(auth.Authenticator{Tokens: database})
	if err != nil {
		tb.Fatal(err)
	}
	var probeCalls atomic.Int32
	probe := func(ctx context.Context) error { probeCalls.Add(1); <-ctx.Done(); return ctx.Err() }
	health := Health{Store: database, VCSCache: &VCSSnapshot{}, ClusterCache: &ClusterSnapshot{}, SecretStoreCache: &SecretStoreProbeSnapshot{},
		VCS: func(ctx context.Context, _ model.Upstream) error { return probe(ctx) }, Cluster: probe,
		SecretStore: &SecretStoreStatus{Configured: true}, SecretStoreProbe: probe, Audit: func(context.Context) error { return nil }}
	control, err := service.NewAPI(service.APIConfig{Runs: database, History: database, Logs: logs, Repositories: database, Health: health, SchedulingObserver: &latencyObserver{}, ResourceDeclarations: &latencyDemand{}})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := control.Shutdown(context.Background()); err != nil {
			tb.Error(err)
		}
	})
	server, err := api.New(authenticator, control, control, "test")
	if err != nil {
		tb.Fatal(err)
	}
	return apiLatencyFixture{handler: server.Handler(), health: health, control: control, probes: &probeCalls, token: strings.TrimSpace(token.String())}
}

func (f apiLatencyFixture) request(tb testing.TB, path string) *httptest.ResponseRecorder {
	tb.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+f.token)
	result := httptest.NewRecorder()
	pprof.Do(request.Context(), pprof.Labels("handler", path), func(ctx context.Context) { f.handler.ServeHTTP(result, request.WithContext(ctx)) })
	if result.Code != http.StatusOK {
		tb.Fatalf("%s: %d %s", path, result.Code, result.Body)
	}
	return result
}

// Cold, stale and inflight snapshots all share the same request latency bound.
// External dependencies deliberately stay blocked until their contexts end.
func TestAPIHandlersLargeHistoryLatency(t *testing.T) {
	f := newAPILatencyFixture(t)
	for _, phase := range []string{"cold", "inflight", "stale"} {
		var stop context.CancelFunc
		var done chan struct{}
		if phase == "inflight" {
			ctx, cancel := context.WithCancel(t.Context())
			stop = cancel
			done = make(chan struct{})
			go func() { defer close(done); f.health.WatchStatusProbes(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			deadline := time.Now().Add(5 * time.Second)
			for f.probes.Load() < 3 {
				if time.Now().After(deadline) {
					t.Fatal("background probes did not start")
				}
				time.Sleep(time.Millisecond)
			}
		}
		if phase == "stale" {
			cache := f.health.ClusterCache
			cache.mu.Lock()
			cache.state, cache.checkedAt = "ready", time.Now().Add(-time.Hour)
			cache.mu.Unlock()
			f.health.SecretStoreCache.mu.Lock()
			f.health.SecretStoreCache.age = time.Now().Add(-time.Hour)
			f.health.SecretStoreCache.mu.Unlock()
			f.health.VCSCache.mu.Lock()
			for i := range f.health.VCSCache.results {
				f.health.VCSCache.results[i].CheckedAt = time.Now().Add(-time.Hour)
			}
			f.health.VCSCache.mu.Unlock()
		}
		for _, path := range []string{"/api/status", "/api/runs?limit=100&latest_per_ref=1", "/api/runs?limit=100"} {
			started := time.Now()
			response := f.request(t, path)
			elapsed := time.Since(started)
			t.Logf("%s %s: %s", phase, path, elapsed)
			if elapsed >= 500*time.Millisecond {
				t.Fatalf("handler exceeded 500ms: %s", elapsed)
			}
			if strings.HasPrefix(path, "/api/runs") {
				var page struct {
					Runs []model.Run `json:"runs"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if len(page.Runs) != 160 {
					t.Fatalf("active runs omitted: %d", len(page.Runs))
				}
			}
		}
		started := time.Now()
		if _, err := f.control.CallTool(t.Context(), api.Actor{Identity: "fixture", Admin: true}, "system_status", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
			t.Fatalf("MCP status exceeded 500ms: %s", elapsed)
		}
		if stop != nil {
			stop()
			<-done
		}
	}
}

func BenchmarkAPIHandlersLargeHistory(b *testing.B) {
	f := newAPILatencyFixture(b)
	for _, path := range []string{"/api/status", "/api/runs?limit=100&latest_per_ref=1"} {
		b.Run(path, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				f.request(b, path)
			}
		})
	}
}
