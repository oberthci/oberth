package sourcedelivery_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestTransferWaitRechecksRunAfterCapacityRelease(t *testing.T) {
	t.Parallel()
	synctest.Test(t, checkTransferWaitReauthorization)
}

func checkTransferWaitReauthorization(t *testing.T) {
	config := deliveryRulesConfig()
	run := config.Run
	var reads, stages atomic.Int32
	var terminal atomic.Bool
	rechecked := make(chan struct{})
	config.Run = func(ctx context.Context, id string) (sourcedelivery.Run, error) {
		r, err := run(ctx, id)
		r.Terminal = terminal.Load()
		if reads.Add(1) == 4 {
			close(rechecked)
		}
		return r, err
	}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	config.Stage = func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
		stages.Add(1)
		entered <- struct{}{}
		<-release
		return sourcedelivery.Staged{Body: io.NopCloser(strings.NewReader("source")), Bytes: 6}, nil
	}
	server := sourcedelivery.New(config)
	descriptor := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}
	responses := make(chan int, 3)
	var workers sync.WaitGroup
	var once sync.Once
	join := func() { once.Do(func() { close(release) }); workers.Wait() }
	t.Cleanup(join)
	for range 2 {
		workers.Go(func() { responses <- requestRulesSource(t, server, descriptor).Code })
	}
	awaitSourceStages(t, entered, responses)
	workers.Go(func() { responses <- requestRulesSource(t, server, descriptor).Code })
	// All three workers are durably blocked: two inside Stage and the third
	// waiting for a transfer slot, after its first authorization completed.
	synctest.Wait()
	if reads.Load() != 3 || stages.Load() != 2 || len(responses) != 0 {
		t.Fatal("third request did not wait behind the active transfers", reads.Load(), stages.Load(), len(responses))
	}
	terminal.Store(true)
	join()
	select {
	case <-rechecked:
	default:
		t.Fatal("capacity waiter did not re-authorize after release")
	}
	forbidden := 0
	for range 3 {
		if code := <-responses; code == http.StatusForbidden {
			forbidden++
		} else if code != http.StatusOK {
			t.Fatal("unexpected load status", code)
		}
	}
	if forbidden != 1 || stages.Load() != 2 {
		t.Fatal("capacity wait used stale run authorization", forbidden, stages.Load())
	}
}
