package service_test

import (
	"context"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

type scopeReadHook struct {
	service.CatalogStore
	read func(context.Context, []string) ([]string, error)
}

func (h scopeReadHook) RepositoryScope(ctx context.Context, scope []string) ([]string, error) {
	return h.read(ctx, scope)
}

func TestEventTimeoutPreservesSnapshotAuthority(t *testing.T) {
	for _, boundary := range []string{"scope-read", "event-read"} {
		for _, change := range []string{"revoke", "hide-repository"} {
			t.Run(boundary+"/"+change, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) { checkEventTimeoutAuthority(t, boundary, change) })
			})
		}
	}
}

func checkEventTimeoutAuthority(t *testing.T, boundary, change string) {
	t.Helper()
	f := setup(t)
	f.seed("running")
	reads, changed := 0, false
	mutate := func(ctx context.Context) {
		changed = true
		changeDeliveryAuthority(t, f, ctx, change)
		f.store.native.SeedEvent(event(0))
	}
	f.c.Catalog = scopeReadHook{CatalogStore: f.store, read: func(ctx context.Context, scope []string) ([]string, error) {
		value, err := f.store.RepositoryScope(ctx, scope)
		if boundary == "scope-read" && reads == 1 && !changed {
			mutate(ctx)
		}
		return value, err
	}}
	f.c.Events = eventHook{EventStore: f.store, read: func(ctx context.Context, q stateport.EventFilter) (model.EventPage, error) {
		value, err := f.store.Events(ctx, q)
		reads++
		if boundary == "event-read" && reads == 2 {
			mutate(ctx)
		}
		return value, err
	}}
	f.restart(t)
	result, err := f.s.EventsWait(t.Context(), caller(), eventsInput{RunIDs: []string{runID}, Timeout: time.Second})
	if !changed {
		t.Fatal("authority mutation did not run")
	}
	if err == nil || !reflect.DeepEqual(result, apicontract.EventResult{}) {
		t.Fatalf("event timeout delivered stale-scope snapshot: reads=%d result=%+v, err=%v", reads, result, err)
	}
}
