package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// A publication whose cache record waits behind a pending receive says so,
// while it retries and when its budget ends: the forge may already have the
// commit, so "failed to publish" alone would be untrue.
func TestPublicationBehindAPendingReceiveSaysSo(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		pending := publicerr.New(publicerr.Unavailable, "a receive is awaiting delivery", "Retry or abandon the recorded receive before changing refs.", nil)
		pending.ID = gitport.ReceivePendingID
		return gitport.PublicationReceipt{}, pending
	}
	out := promote(t, f)
	id := "publication:" + out.ID
	for attempt := 1; attempt <= 5; attempt++ {
		if err := f.s.Deliver(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		p, err := f.store.Publication(t.Context(), id)
		if err != nil || p.Reason != "waiting for a pending receive" {
			t.Fatal("attempt", attempt, "does not say it waits behind a receive", p.Reason, err)
		}
		if final := attempt == 5; (p.Status == model.PublicationFailed) != final {
			t.Fatal("wrong state after attempt", attempt, p.Status)
		}
		f.offset.Add(int64(time.Hour))
	}
}
