package sourcedelivery_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestTransferWaitRechecksRunAfterCapacityRelease(t *testing.T) {
	t.Parallel()
	config := deliveryRulesConfig()
	run := config.Run
	var reads, stages atomic.Int32
	var terminal atomic.Bool
	seen := make(chan struct{})
	config.Run = func(ctx context.Context, id string) (sourcedelivery.Run, error) {
		r, err := run(ctx, id)
		r.Terminal = terminal.Load()
		if reads.Add(1) == 3 {
			close(seen)
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
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("waiting request was not authorized")
	}
	terminal.Store(true)
	join()
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
