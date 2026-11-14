package service

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

// TestMCPUnderQueueLoad runs the real scheduler, SQLite event recorder and HTTPS
// MCP handler. Only Git and the execution backend are fakes: ten Workflows stay
// in Wait and 64 ordinary runs have not yet been dispatched. Each of eight
// subscribers follows a full retained log while events arrive at 10/s; run_list
// is requested at 20/s; every tenth request closes its TLS connection.
// -server-load-duration extends the measured phase; -server-load-profile saves a
// CPU profile of that phase only, excluding database/fixture construction.
var serverLoadDuration = flag.Duration("server-load-duration", 3*time.Second, "duration of the MCP load test")
var serverLoadProfile = flag.String("server-load-profile", "", "CPU profile of the MCP load phase")

func TestMCPUnderQueueLoad(t *testing.T) {
	f, service := queueLoadFixture(t)
	server := httptest.NewTLSServer(resultServer(t, service, agentActor).Handler())
	defer server.Close()
	client := server.Client()
	client.Transport.(*http.Transport).MaxIdleConnsPerHost = 16
	client.Timeout = testTimeout(2 * time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	failures := make(chan error, 16)
	defer func() { cancel(); workers.Wait() }()
	ready := make(chan struct{}, 8)
	var deliveries [8]atomic.Int64
	for index := range 8 {
		workers.Go(func() {
			cursor := int64(DefaultAgentEventRetention)
			ready <- struct{}{}
			for ctx.Err() == nil {
				started := time.Now()
				raw, err := loadMCPCall(ctx, client, server.URL, "events_wait", fmt.Sprintf(`{"after_id":%d,"timeout_s":1}`, cursor), false)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					failures <- err
					return
				}
				var page EventsWaitResponse
				if err := json.Unmarshal(raw, &page); err != nil {
					failures <- err
					return
				}
				if len(page.Events) > 0 {
					if time.Since(started) > testTimeout(time.Second) {
						failures <- fmt.Errorf("event delivery exceeded one second")
						return
					}
					deliveries[index].Add(1)
				}
				cursor = page.NextAfterID
			}
		})
	}
	for range 8 {
		<-ready
	}
	duration := *serverLoadDuration
	if duration <= 0 {
		t.Fatal("load duration must be positive")
	}
	if path := *serverLoadProfile; path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.StartCPUProfile(file); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		defer func() { pprof.StopCPUProfile(); _ = file.Close() }()
	}

	workers.Go(func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for sequence := 0; ; sequence++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_, _, err := f.store.AppendAgentEvent(ctx, model.AgentEvent{Key: fmt.Sprintf("load-live-%d", sequence), Kind: model.AgentEventRunFinished, RepoID: f.terraform.ID, Repo: "codeberg/acme/terraform", Ref: "feature/load", Summary: "synthetic completion"}, DefaultAgentEventRetention)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					failures <- err
					return
				}
				service.agentEvents.broadcast()
			}
		}
	})
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	var latencies []time.Duration
	for {
		select {
		case err := <-failures:
			t.Fatal(err)
		case <-deadline.C:
			if len(latencies) == 0 {
				t.Fatal("no MCP samples")
			}
			cancel()
			workers.Wait()
			select {
			case err := <-failures:
				t.Fatal(err)
			default:
			}
			for index := range deliveries {
				if deliveries[index].Load() == 0 {
					t.Fatalf("subscriber %d received no events", index)
				}
			}
			slices.Sort(latencies)
			t.Logf("requests=%d p50=%s p95=%s max=%s", len(latencies), latencies[len(latencies)/2], latencies[len(latencies)*95/100], latencies[len(latencies)-1])
			assertQueueLoad(t, f)
			return
		case <-tick.C:
			started := time.Now()
			_, err := loadMCPCall(ctx, client, server.URL, "run_list", `{"limit":100}`, len(latencies)%10 == 0)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatal(err)
			}
			if elapsed > testTimeout(time.Second) {
				t.Fatalf("MCP latency %s exceeds one-second budget", elapsed)
			}
			latencies = append(latencies, elapsed)
		}
	}
}

func queueLoadFixture(t *testing.T) (*groupFixture, *API) {
	t.Helper()
	f := newGroupFixture(t, 10)
	service, err := NewAPI(APIConfig{Runs: f.store, History: f.store, Repositories: f.store, Issues: f.store, Promotions: f.store, PromotionRuns: f.store, Enqueues: fixtureEnqueues{f.scheduler}, Logs: f.control.logs, Auditor: f.store, Signals: f.scheduler.signals, AgentEvents: f.store, MaximumWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	for index := range 10 {
		f.branch(t, f.terraform, fmt.Sprintf("feature/running-%d", index), "a")
	}
	for range 10 {
		run, err := storetest.ClaimNextRun(f.store, t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.SetRunWorkflowName(t.Context(), run.ID, "oberth-"+run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.MarkRunSubmissionStarted(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.MarkRunSubmitted(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 64 {
		f.branch(t, f.terraform, fmt.Sprintf("feature/pending-%d", index), "b")
	}
	for index := range DefaultAgentEventRetention {
		_, _, err := f.store.AppendAgentEvent(t.Context(), model.AgentEvent{Key: fmt.Sprintf("load-history-%d", index), Kind: model.AgentEventRunFinished, RepoID: f.terraform.ID, Repo: "codeberg/acme/terraform", Summary: "synthetic history"}, DefaultAgentEventRetention)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := service.StartAgentEvents(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertQueueLoad(t, f)
	return f, service
}

func assertQueueLoad(t *testing.T, f *groupFixture) {
	t.Helper()
	runs, err := f.store.ListActiveRuns(t.Context(), model.RunListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[model.RunStatus]int{}
	for _, run := range runs {
		counts[run.Status]++
	}
	if counts[model.RunQueued] != 0 || counts[model.RunRunning] != 74 {
		t.Fatalf("load changed: %v", counts)
	}
}

func loadMCPCall(ctx context.Context, client *http.Client, url, tool, args string, reconnect bool) (json.RawMessage, error) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/mcp", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	request.Close = reconnect
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError    bool            `json:"isError"`
			Structured json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK || len(envelope.Error) > 0 || envelope.Result.IsError || len(envelope.Result.Structured) == 0 {
		return nil, fmt.Errorf("%s failed: %s", tool, raw)
	}
	return envelope.Result.Structured, nil
}
