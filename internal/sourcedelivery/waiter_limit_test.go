package sourcedelivery_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestWorkflowHasAtMostSixteenSourceWaiters(t *testing.T) {
	synctest.Test(t, checkWorkflowWaiters)
}

func checkWorkflowWaiters(t *testing.T) {
	config := deliveryRulesConfig()
	authenticate, run := config.Authenticate, config.Run
	config.Authenticate = func(ctx context.Context, value string) (sourcedelivery.Caller, error) {
		caller, err := authenticate(ctx, value)
		if value == "other" {
			caller.Workflow, caller.UID = "other", "other-uid"
		}
		return caller, err
	}
	config.Run = func(ctx context.Context, id string) (sourcedelivery.Run, error) {
		r, err := run(ctx, id)
		if id == "other" {
			r.Workflow, r.UID, r.Admission.RunID = "other", "other-uid", "other"
		}
		return r, err
	}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var stages atomic.Int32
	config.Stage = func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
		if stages.Add(1) <= 2 {
			entered <- struct{}{}
		}
		<-release
		return sourcedelivery.Staged{Body: io.NopCloser(strings.NewReader("source")), Bytes: 6}, nil
	}
	server := sourcedelivery.New(config)
	responses := make(chan int, 19)
	var workers sync.WaitGroup
	var once sync.Once
	join := func() { once.Do(func() { close(release) }); workers.Wait() }
	t.Cleanup(join)
	for range 2 {
		workers.Go(func() { responses <- waiterLoad(t, server, t.Context(), waiterRequest{"run", "bound"}).Code })
		<-entered
	}
	for range 16 {
		workers.Go(func() { responses <- waiterLoad(t, server, t.Context(), waiterRequest{"run", "bound"}).Code })
		synctest.Wait()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	overflow := waiterLoad(t, server, ctx, waiterRequest{"run", "bound"})
	if overflow.Code != 503 || overflow.Header().Get("Retry-After") != "1" {
		t.Fatal("Workflow kept another waiter", overflow.Code, overflow.Header())
	}
	workers.Go(func() { responses <- waiterLoad(t, server, t.Context(), waiterRequest{"other", "other"}).Code })
	synctest.Wait()
	if len(responses) != 0 || stages.Load() != 2 {
		t.Fatal("waiters bypassed capacity or one Workflow blocked another", len(responses), stages.Load())
	}
	join()
	for range 19 {
		if code := <-responses; code != http.StatusOK {
			t.Fatal("bounded waiter failed after release", code)
		}
	}
}

type waiterRequest struct{ locator, bearer string }

func waiterLoad(t *testing.T, server *sourcedelivery.Server, ctx context.Context, request waiterRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(sourcedelivery.LoadRequest{Locator: request.locator, Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, sourcedelivery.LoadPath, strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+request.bearer)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	return w
}
