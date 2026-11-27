package service_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestWaitShutdownDiscardsSnapshot(t *testing.T) {
	f := setup(t)
	f.seed("running")
	reads := 0
	f.c.Runs = stateReadHook{RunStore: f.store, read: func(ctx context.Context, id string) (stateport.RunState, error) {
		value, err := f.store.RunState(ctx, id)
		reads++
		f.store.mu.Lock()
		delete(f.store.Actors, caller().Fingerprint)
		f.store.mu.Unlock()
		if err := f.s.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		return value, err
	}}
	f.restart(t)
	result, err := f.s.RunWait(t.Context(), caller(), viewQuery{ID: runID}, time.Minute)
	requireCode(t, err, publicerr.Unavailable)
	if reads != 1 || !reflect.DeepEqual(result, apicontract.WaitResult{}) {
		t.Fatalf("shutdown delivered snapshot: reads=%d result=%+v", reads, result)
	}
}
