package gitcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveDropsCacheAndGatesEveryRefresh(t *testing.T) {
	t.Parallel()
	cache := newTestCache(t, "unused")
	path := filepath.Join(cache.root, "widget.git")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	archived := false
	refused := errors.New("repository archived")
	cache.repositoryGate = func(context.Context, string) error {
		if archived {
			return refused
		}
		return nil
	}
	err := cache.ChangeRepositoryArchive(context.Background(), "widget", true, func() error { archived = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache retained: %v", err)
	}
	if _, err := cache.LsRemoteHeads(context.Background(), "widget"); !errors.Is(err, refused) {
		t.Fatalf("archived probe = %v", err)
	}
	if _, _, err := cache.RemoteRef(context.Background(), "widget", "refs/heads/main"); !errors.Is(err, refused) {
		t.Fatalf("archived remote ref probe = %v", err)
	}
	for _, ensure := range []func(context.Context, string) (Repository, error){cache.Ensure, cache.EnsureForFetch} {
		if _, err := ensure(context.Background(), "widget"); !errors.Is(err, refused) {
			t.Fatalf("archived refresh = %v", err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache recreated: %v", err)
	}
	if err := cache.ChangeRepositoryArchive(context.Background(), "widget", false, func() error { archived = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if archived {
		t.Fatal("unarchive did not transition")
	}
}

func TestArchiveRefusalPreservesCacheAndPendingReceive(t *testing.T) {
	t.Parallel()
	cache := newTestCache(t, "unused")
	path := filepath.Join(cache.root, "widget.git")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("active runs")
	if err := cache.ChangeRepositoryArchive(context.Background(), "widget", true, func() error { return refused }); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("refused archive removed cache")
	}
	reservation := receiveReservation{Version: receiveReservationVersion, ID: "pending", Repo: "widget", Actor: "actor", State: receiveReserved, Before: map[string]string{}}
	if err := cache.writeReservation(reservation); err != nil {
		t.Fatal(err)
	}
	changed := false
	if err := cache.ChangeRepositoryArchive(context.Background(), "widget", true, func() error { changed = true; return nil }); err == nil || changed {
		t.Fatalf("pending receive allowed archive: %v", err)
	}
	if _, err := cache.readReservation("widget"); err != nil {
		t.Fatal("archive lost receive evidence")
	}
}
