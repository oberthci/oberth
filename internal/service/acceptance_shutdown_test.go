package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type blockedStartStore struct {
	*store.Store
	entered, release chan struct{}
}

func (s *blockedStartStore) StartRun(ctx context.Context, id string) (model.Run, error) {
	close(s.entered)
	<-s.release
	return s.Store.StartRun(ctx, id)
}
func TestAcceptanceStoreWriteDoesNotBlockShutdown(t *testing.T) {
	f := newControlFixture(t)
	accepted, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "shutdown-write", Repository: f.repo, Branch: "main", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockedStartStore{Store: f.store, entered: make(chan struct{}), release: make(chan struct{})}
	f.scheduler.store = blocked
	acceptDone := make(chan error, 1)
	go func() { acceptDone <- f.scheduler.AcceptEnqueue(t.Context(), accepted) }()
	<-blocked.entered
	defer close(blocked.release)
	shutdown := make(chan struct{})
	go func() { f.scheduler.closeObservations(); close(shutdown) }()
	select {
	case <-shutdown:
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("database write held the observation shutdown lock")
	}
	// The accepting goroutine may now finish, but cannot register after shutdown.
	t.Cleanup(func() {
		<-acceptDone
		if f.scheduler.submissionStateForTest().Runs != 0 {
			t.Error("submission registered after shutdown")
		}
	})
}
