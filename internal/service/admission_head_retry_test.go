package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// A liveness tick must not bypass the persistence retry delay.
func TestHeadWaitPersistenceRetriesWithoutAnotherEvent(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprint("clear=", clear), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := newWeightedAdmission(8)
				g.headEvents = make(chan headEvent, 1)
				state := &retryTimerHeadStore{attempts: make(chan headEvent, 4)}
				s := &Scheduler{store: state, admission: g}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { defer close(done); s.runHeadEvents(ctx) }()
				defer func() { cancel(); <-done }()
				synctest.Wait()
				time.Sleep(time.Second)
				key := headWaitKey{RepoID: 1, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40)}
				g.mu.Lock()
				g.emitHeadLocked(headEvent{key: key, since: time.Now(), clear: clear})
				g.mu.Unlock()
				<-state.attempts
				synctest.Wait()
				time.Sleep(29 * time.Second)
				synctest.Wait()
				select {
				case <-state.attempts:
					t.Fatal("liveness tick retried persistence before the retry delay")
				default:
				}
				time.Sleep(time.Second)
				synctest.Wait()
				select {
				case event := <-state.attempts:
					if event.clear != clear || event.key != key {
						t.Fatalf("retry changed event: %+v", event)
					}
				default:
					t.Fatal("failed write did not retry after 30 seconds without another event")
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				select {
				case <-state.attempts:
					t.Fatal("successfully persisted event was retried")
				default:
				}
			})
		})
	}
}

type retryTimerHeadStore struct {
	SchedulerStore
	HeadWaitStore
	attempts chan headEvent
	failed   bool
}

func (s *retryTimerHeadStore) persist(event headEvent) error {
	s.attempts <- event
	if !s.failed {
		s.failed = true
		return errors.New("injected persistence failure")
	}
	return nil
}

func (s *retryTimerHeadStore) RecordAdmissionHeadWait(_ context.Context, repo int64, kind model.RefKind, ref, sha string, since time.Time) error {
	return s.persist(headEvent{key: headWaitKey{repo, kind, ref, sha}, since: since})
}

func (s *retryTimerHeadStore) ClearAdmissionHeadWait(_ context.Context, repo int64, kind model.RefKind, ref, sha string) error {
	return s.persist(headEvent{key: headWaitKey{repo, kind, ref, sha}, clear: true})
}
