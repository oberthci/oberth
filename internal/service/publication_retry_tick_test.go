package service

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPublicationRetryBackoffTicksDoNotLogAttempts(t *testing.T) {
	f, pub := pendingAbandonFixture(t)
	git := &policyTestGit{controlGit: f.git}
	f.scheduler.git = git
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	for range 3 {
		state, err := f.scheduler.DeliverPublication(t.Context(), pub.ID)
		if err != nil || state.Publication.Status != model.PublicationPending {
			t.Fatalf("backoff state: %+v %v", state, err)
		}
	}
	if strings.Contains(logs.String(), "publication attempt") || strings.Contains(logs.String(), "publication outcome") {
		t.Fatalf("idle retry ticks logged attempts: %s", logs.String())
	}
	if len(git.reads) != 0 {
		t.Fatalf("backoff re-evaluated trusted policy: %v", git.reads)
	}
	if ids := f.scheduler.takePublicationRecoveries(); len(ids) != 1 || ids[0] != pub.ID {
		t.Fatalf("backoff lost retry wake: %v", ids)
	}
	// The due attempt still checks policy, pushes exactly once, and logs evidence.
	if _, err := f.store.UpdatePublicationAttempt(t.Context(), pub.ID, pub.Attempts, pub.Attempts, time.Now().Add(-time.Second), pub.Error); err != nil {
		t.Fatal(err)
	}
	final, err := f.scheduler.DeliverPublication(t.Context(), pub.ID)
	if err != nil || final.Publication.Status != model.PublicationDelivered {
		t.Fatalf("due delivery: %+v %v", final, err)
	}
	if len(git.reads) == 0 || len(f.git.syncedBranches) != 1 {
		t.Fatalf("due delivery skipped policy or push: reads=%v pushes=%v", git.reads, f.git.syncedBranches)
	}
	if !strings.Contains(logs.String(), "publication attempt") || !strings.Contains(logs.String(), "status=delivered") {
		t.Fatalf("due attempt lost evidence: %s", logs.String())
	}
}
