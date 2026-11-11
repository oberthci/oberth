package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
)

type blockingWaitBackend struct {
	*fakeBackend
	release chan struct{}
}

func (backend *blockingWaitBackend) CallTool(ctx context.Context, _ Actor, _ string, _ json.RawMessage) (any, error) {
	select {
	case <-backend.release:
	case <-ctx.Done():
	}
	return map[string]any{"state": "done"}, nil
}

func TestMCPLongPollsAreCappedPerCredential(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	backend := &blockingWaitBackend{fakeBackend: &fakeBackend{}, release: release}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	server.longPolls.limit = 2
	var done sync.WaitGroup
	for range 2 {
		done.Add(1)
		go func() {
			defer done.Done()
			server.Handler().ServeHTTP(httptest.NewRecorder(), streamRequest(waitRPC, "application/json", "valid-token"))
		}()
	}
	deadline := time.Now().Add(testtimeout.Scale(5 * time.Second))
	for {
		server.longPolls.mu.Lock()
		active := 0
		for _, count := range server.longPolls.active {
			active += count
		}
		server.longPolls.mu.Unlock()
		if active == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("long polls never started: %d", active)
		}
		time.Sleep(time.Millisecond)
	}
	refused := httptest.NewRecorder()
	server.Handler().ServeHTTP(refused, streamRequest(waitRPC, "application/json", "valid-token"))
	if body := refused.Body.String(); !strings.Contains(body, `"isError":true`) || !strings.Contains(body, "already has 2 blocking calls open") || !strings.Contains(body, `"exact-request"`) {
		t.Fatalf("third long poll was not refused: %s", body)
	}
	close(release)
	done.Wait()
	server.longPolls.mu.Lock()
	left := len(server.longPolls.active)
	server.longPolls.mu.Unlock()
	if left != 0 {
		t.Fatalf("slots leaked: %d", left)
	}
	again := httptest.NewRecorder()
	server.Handler().ServeHTTP(again, streamRequest(waitRPC, "application/json", "valid-token"))
	if strings.Contains(again.Body.String(), "blocking calls open") {
		t.Fatalf("slot not returned: %s", again.Body.String())
	}
}

// Hold actions outlast the server-wide WriteTimeout; their write deadline
// follows the tool's own bound. Other tools keep the server default.
func TestToolWriteBoundCoversHoldActions(t *testing.T) {
	exec := toolTimeouts()["run_hold_exec"].Default
	if exec < 10*time.Minute || toolWriteBound("run_hold_exec") != exec || toolWriteBound("run_hold_patch") != 2*exec || toolWriteBound("run_get") != 0 {
		t.Fatalf("exec %s patch %s run_get %s", toolWriteBound("run_hold_exec"), toolWriteBound("run_hold_patch"), toolWriteBound("run_get"))
	}
}
