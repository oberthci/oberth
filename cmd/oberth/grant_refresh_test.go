package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type blockedGrantIdentityStore struct {
	fakePerRepoStore
	reads   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (s *blockedGrantIdentityStore) SecretAccessList(ctx context.Context, qualified string, pending bool) ([]store.SecretAccessGrant, error) {
	if s.reads.Add(1) == 1 {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.fakePerRepoStore.SecretAccessList(ctx, qualified, pending)
}

func TestRefreshGrantIdentitiesSerializesBuildAndPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	database := &blockedGrantIdentityStore{
		fakePerRepoStore: fakePerRepoStore{
			repos:     []model.Repository{{ID: 1, Name: "repo", UpstreamID: 1}},
			upstreams: []model.Upstream{{ID: 1, Name: "upstream", BaseURL: "ssh://git@upstream.example.test/org"}},
			grants:    map[int64]map[string]map[string]bool{1: {"build": {"oberth/data/upstream/org/repo/token": true}}},
		},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	identities := argojob.NewIdentityStoreWithSteps(nil, nil, nil)
	refresh := newGrantIdentityRefresher(database, identities)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- refresh(ctx) }()
	<-database.entered
	started := make(chan struct{})
	go func() {
		close(started)
		second <- refresh(ctx)
	}()
	<-started
	completed := false
	select {
	case err := <-second:
		completed = true
		t.Errorf("concurrent refresh bypassed the unfinished build: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if reads := database.reads.Load(); reads != 1 {
		t.Errorf("concurrent refresh read the database during an unfinished build: %d reads", reads)
	}
	if len(identities.Snapshot().Step) != 0 {
		t.Error("concurrent refresh published before the earlier build finished")
	}
	close(database.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	// The early completion path already consumed second's result.
	if completed {
		return
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	snapshot := identities.Snapshot()
	key := "upstream/org/repo/.oberth/release.yaml/build"
	if len(snapshot.Release) != 1 || len(snapshot.CI) != 1 || snapshot.Step[key].CIServiceAccountName == "" || snapshot.Step[key].ServiceAccountName == "" {
		t.Fatalf("refresh did not publish all identity maps: %+v", snapshot)
	}
}
