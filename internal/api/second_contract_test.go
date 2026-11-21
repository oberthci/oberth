package api_test

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"

	"github.com/oberthci/oberth/internal/api/contracttest"
	"github.com/oberthci/oberth/internal/api/fake"
	"sync"
	"testing"
)

func TestSecondCooperativeExecutionAndJoinContract(t *testing.T) {
	contracttest.HandlerCancellation(t, contracttest.CancellationCase[apicontract.RunWaitArgs, apicontract.WaitResult]{Caller: admin(), Input: apicontract.RunWaitArgs{ID: "12345678"}, New: func(*testing.T) contracttest.BlockingHandler[apicontract.RunWaitArgs, apicontract.WaitResult] {
		started, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		h := &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Run: func(ctx context.Context) (apicontract.WaitResult, error) {
			defer close(stopped)
			close(started)
			select {
			case <-ctx.Done():
				return apicontract.WaitResult{}, ctx.Err()
			case <-release:
				return apicontract.WaitResult{}, nil
			}
		}}
		return contracttest.BlockingHandler[apicontract.RunWaitArgs, apicontract.WaitResult]{Handler: h, Started: started, Stopped: stopped, Release: sync.OnceFunc(func() { close(release) })}
	}})
}
