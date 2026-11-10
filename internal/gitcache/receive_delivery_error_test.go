package gitcache

import (
	"context"
	"errors"
	"testing"
)

// A failed run-creation callback names the stuck record. The pusher whose
// refs were accepted sees Earlier=false; the next pusher, blocked before its
// transport by the same record, sees Earlier=true and the repository.
func TestReceiveDeliveryErrorNamesTheStuckRecord(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	callbackFailure := errors.New("create run: database is locked")
	failing := ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return callbackFailure })
	first := repository.featureCommit(t, "feature/first", "first\n")
	seedCacheObject(t, repository.work, ready.Path, first)
	err = cache.receiveTransaction(context.Background(), "example", ready.Path, "actor-a", func() error {
		return cache.updateRef(context.Background(), ready.Path, "refs/heads/feature/first", first, "")
	}, failing)
	var delivery *ReceiveDeliveryError
	if !errors.As(err, &delivery) || delivery.Earlier || delivery.Ref != "refs/heads/feature/first" || delivery.EventID == "" || !errors.Is(err, callbackFailure) {
		t.Fatalf("own delivery failure = %#v (%v)", delivery, err)
	}
	transported := false
	err = cache.receiveTransaction(context.Background(), "example", ready.Path, "actor-b", func() error {
		transported = true
		return nil
	}, failing)
	var blocked *ReceiveBlockedError
	if !errors.As(err, &blocked) || blocked.Repo == "" || !errors.As(err, &delivery) || !delivery.Earlier || delivery.Ref != "refs/heads/feature/first" {
		t.Fatalf("blocked push = %#v %#v (%v)", blocked, delivery, err)
	}
	if transported {
		t.Fatal("a blocked push ran its transport")
	}
}
