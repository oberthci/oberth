package service

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestPublishRetryReturnsPendingUntilDurableFinalization(t *testing.T) {
	ctx := context.Background()
	f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}})
	queued, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "pending-publication", Repository: f.repo, Branch: "feature/pending-publication", SHA: strings.Repeat("a", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	f.git.remoteRefErr = errors.New("forge unavailable")
	if err := f.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	f.git.remoteRefErr = nil
	f.scheduler.store = &finalizeFailStore{Store: f.store, failNext: true}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	response, err := f.api(t).publishRetry(ctx, api.Actor{Identity: "agent@host", Admin: true}, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != string(model.PublicationPending) || response.PublicationID == "" || response.RunID != queued.ID {
		t.Fatalf("ambiguous finalization reported terminal: %#v", response)
	}
	publication, err := f.store.Publication(ctx, response.PublicationID)
	if err != nil || publication.Status != model.PublicationPending {
		t.Fatalf("lost pending intent: %#v %v", publication, err)
	}
	if err := f.scheduler.recoverNotifiedPublicationsWithGate(ctx, f.scheduler.requireMutation); err != nil {
		t.Fatal(err)
	}
	publication, err = f.store.Publication(ctx, response.PublicationID)
	if err != nil || publication.Status != model.PublicationDelivered {
		t.Fatalf("intent not recovered: %#v %v", publication, err)
	}
	run, err := f.store.Run(ctx, queued.ID)
	if err != nil || run.Status != model.RunPassed {
		t.Fatalf("run not finalized: %#v %v", run, err)
	}
	if len(f.git.syncedBranches) != 1 {
		t.Fatalf("recovery repeated delivered push: %v", f.git.syncedBranches)
	}
	for _, marker := range []string{"publication attempt run=" + queued.ID + " publication=" + response.PublicationID, "publication outcome run=" + queued.ID + " publication=" + response.PublicationID + " status=unconfirmed", "publication outcome run=" + queued.ID + " publication=" + response.PublicationID + " status=delivered"} {
		if !strings.Contains(logs.String(), marker) {
			t.Fatalf("missing publication evidence %q: %s", marker, logs.String())
		}
	}
}
