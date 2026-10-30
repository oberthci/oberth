package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestPatternScanLimitReservesOneSlotPerTokenBeforeGlobal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := patternScanLimiter{global: make(chan struct{}, 2)}
		ctxA := withRunLogCaller(t.Context(), api.Actor{TokenCredentialID: "a", Identity: "same", Fingerprint: "same"})
		ctxB := withRunLogCaller(t.Context(), api.Actor{TokenCredentialID: "b", Identity: "same", Fingerprint: "same"})
		releaseA, err := limiter.acquire(ctxA)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseA()
		next := make(chan func(), 1)
		go func() {
			release, err := limiter.acquire(withRunLogCaller(t.Context(), api.Actor{TokenCredentialID: "a", Identity: "renamed", Fingerprint: "changed"}))
			if err != nil {
				t.Error(err)
				return
			}
			next <- release
		}()
		synctest.Wait()
		if len(limiter.global) != 1 {
			t.Fatal("one token consumed both global slots")
		}
		releaseB, err := limiter.acquire(ctxB)
		if err != nil {
			t.Fatal("second token sharing a display identity was blocked", err)
		}
		releaseB()
		synctest.Wait()
		select {
		case release := <-next:
			release()
			t.Fatal("same token scan entered before its first scan finished")
		default:
		}
		releaseA()
		releaseNext := <-next
		releaseA() // a stale/double release cannot release the token's next owner
		if len(limiter.global) != 1 {
			t.Fatal("double release disturbed the new owner")
		}
		releaseNext()
		releaseNext()
		if len(limiter.global) != 0 || len(limiter.owners) != 0 {
			t.Fatal("completed scan leaked state")
		}
	})
}

func TestPatternScanLimitCancellationCleansBothWaitPaths(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := patternScanLimiter{global: make(chan struct{}, 2)}
		ctxA := withRunLogCaller(t.Context(), api.Actor{TokenCredentialID: "a"})
		releaseA, err := limiter.acquire(ctxA)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseA()
		ctx, cancel := context.WithTimeout(ctxA, time.Second)
		started := time.Now()
		if _, err := limiter.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != time.Second {
			t.Fatalf("per-token wait lost deadline: %v", err)
		}
		cancel()
		if len(limiter.global) != 1 || len(limiter.owners) != 1 {
			t.Fatal("canceled token waiter released its live owner")
		}
		releaseB, err := limiter.acquire(withRunLogCaller(t.Context(), api.Actor{TokenCredentialID: "b"}))
		if err != nil {
			t.Fatal(err)
		}
		defer releaseB()
		ctx, cancel = context.WithTimeout(withRunLogCaller(t.Context(), api.Actor{TokenCredentialID: "c"}), time.Second)
		if _, err := limiter.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("global wait lost deadline", err)
		}
		cancel()
		if len(limiter.owners) != 2 || len(limiter.global) != 2 {
			t.Fatal("canceled global waiter leaked its token owner")
		}
		var workers sync.WaitGroup
		queued, stop := context.WithCancel(t.Context())
		for index := range 50 {
			workers.Go(func() {
				_, err := limiter.acquire(withRunLogCaller(queued, api.Actor{TokenCredentialID: fmt.Sprint(index)}))
				if !errors.Is(err, context.Canceled) {
					t.Errorf("queued request: %v", err)
				}
			})
		}
		synctest.Wait()
		stop()
		workers.Wait()
		if len(limiter.owners) != 2 {
			t.Fatal("canceled callers accumulated token state")
		}
		releaseA()
		releaseB()
		if len(limiter.owners) != 0 {
			t.Fatal("owner map did not drain")
		}
	})
}

func TestPatternScanCallerCredentialIsNotPublicActorData(t *testing.T) {
	authenticator, err := NewAuthenticator(stubUplinkAuthenticator{value: model.AuthenticatedUplink{Uplink: model.Uplink{Identity: "reader", Fingerprint: "bound"}, TokenCredential: model.TokenCredential{ID: "credential-id"}}})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := authenticator.Authenticate(t.Context(), "not-retained-plaintext")
	if err != nil || actor.TokenCredentialID != "credential-id" {
		t.Fatal("auth adapter discarded the token binding", err)
	}
	encoded, err := json.Marshal(actor)
	if err != nil || strings.Contains(string(encoded), "credential-id") || strings.Contains(string(encoded), "not-retained-plaintext") {
		t.Fatal("credential admission key leaked", string(encoded), err)
	}
	a := withRunLogCaller(t.Context(), api.Actor{Fingerprint: "legacy"}).Value(runLogCallerKey{})
	b := withRunLogCaller(t.Context(), api.Actor{Identity: "other", Fingerprint: "legacy"}).Value(runLogCallerKey{})
	if a != b {
		t.Fatal("legacy callers could bypass their fingerprint bucket by renaming")
	}
}

type routedPatternLogs struct {
	LogStore
	entered chan string
	release map[string]<-chan struct{}
}

func (logs routedPatternLogs) ReadFiltered(id, burn, step string, filter runlog.Filter) ([]byte, runlog.Meta, error) {
	if filter.Pattern != "" {
		logs.entered <- filter.Pattern
		<-logs.release[filter.Pattern]
	}
	return logs.LogStore.ReadFiltered(id, burn, step, filter)
}

func TestPatternScanTokenLimitSharedByMCPAndDashboardAndPlainReadsBypass(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "feature/token-limit", "branch", true)
	first, second := make(chan struct{}), make(chan struct{})
	var firstOnce, secondOnce sync.Once
	releaseFirst := func() { firstOnce.Do(func() { close(first) }) }
	releaseSecond := func() { secondOnce.Do(func() { close(second) }) }
	defer releaseFirst()
	defer releaseSecond()
	entered := make(chan string, 3)
	f.api.logs = routedPatternLogs{LogStore: f.api.logs, entered: entered, release: map[string]<-chan struct{}{"first": first, "queued": first, "other": second}}
	actorA := api.Actor{Identity: "same", Fingerprint: "same", TokenCredentialID: "token-a"}
	actorB := actorA
	actorB.TokenCredentialID = "token-b"
	results := make(chan error, 3)
	go func() {
		raw, _ := json.Marshal(map[string]any{"id": run.ID, "burn": "test", "step": "unit", "pattern": "first"})
		_, err := f.api.CallTool(t.Context(), actorA, "run_logs", raw)
		results <- err
	}()
	awaitPattern := func(want string) {
		t.Helper()
		select {
		case got := <-entered:
			if got != want {
				t.Fatalf("scan %q entered, expected %q", got, want)
			}
		case <-time.After(testtimeout.Start(5 * time.Second)):
			t.Fatal("pattern did not enter", want)
		}
	}
	awaitPattern("first")
	queued, cancelQueued := context.WithCancel(t.Context())
	defer cancelQueued()
	go func() {
		_, err := f.api.RunLogFiltered(queued, actorA, run.ID, "test", "unit", runlog.Filter{Pattern: "queued"})
		results <- err
	}()
	go func() {
		_, err := f.api.RunLogFiltered(t.Context(), actorB, run.ID, "test", "unit", runlog.Filter{Pattern: "other"})
		results <- err
	}()
	awaitPattern("other") // one actor cannot consume both slots, even across surfaces
	plain, err := f.api.RunLogFiltered(t.Context(), actorA, run.ID, "test", "unit", runlog.Filter{})
	if err != nil || plain.(LogResponse).Output == "" {
		t.Fatal("plain log read waited for a pattern scan", err)
	}
	cancelQueued()
	select {
	case err := <-results:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) {
			t.Fatal("queued scan ignored cancellation", err)
		}
	case <-time.After(testtimeout.Start(5 * time.Second)):
		t.Fatal("token wait did not cancel")
	}
	releaseFirst()
	releaseSecond()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	// Public JSON and MCP structured content must report the same call budget.
	server, err := api.New(runSurfaceAuth{}, f.api, f.api, "test")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID+"/logs?burn=test&step=unit", nil)
	request.Header.Set("Authorization", "Bearer test")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var body LogResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body.ScanLimitBytes != runlog.MaxScanBytes || body.TotalScannedBytes != body.ScannedBytes+body.DiscoveryBytes {
		t.Fatalf("dashboard lost scan accounting: %s", response.Body)
	}
}
