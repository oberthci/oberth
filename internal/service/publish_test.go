package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func finishPublication(t *testing.T, f *fixture, r stateport.Run) model.Publication {
	t.Helper()
	f.store.native.SeedRun(r)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.Publication(t.Context(), "publication:"+r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func workerOK(t *testing.T, f *fixture) {
	t.Helper()
	select {
	case err := <-f.errors:
		t.Fatal("publication worker", err)
	default:
	}
}
func TestFinishedCommitsIntentWithoutUpstreamAvailability(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	reads := 0
	f.moving.Remote = func(context.Context, string, string) (gitport.Ref, error) {
		reads++
		return gitport.Ref{}, &gitport.UnavailableError{Op: "remote"}
	}
	p := finishPublication(t, f, r)
	stored, _ := f.store.Run(t.Context(), r.ID)
	if p.PreviousSHA != nil || p.Status != model.PublicationPending || stored.Status != stepmodel.RunPassed || reads != 0 || f.store.Calls()["Finish"] != 1 {
		t.Fatal("finish was not one local fact", p, stored, reads)
	}
	if !f.timers.Fire() {
		t.Fatal("publication was not armed")
	}
	workerOK(t, f)
	p, _ = f.store.Publication(t.Context(), p.ID)
	if p.Attempts != 1 || p.RetryAt == nil || reads != 1 {
		t.Fatal("outage lost durable retry", p, reads)
	}
}
func TestPublicationRetryBudgetAndAbandon(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	f.moving.Remote = func(context.Context, string, string) (gitport.Ref, error) {
		return gitport.Ref{}, &gitport.UnavailableError{Op: "remote"}
	}
	p = exhaustPublication(t, f, p)
	call, err := f.s.PreparePublishRetry(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PublishRetryArgs{ID: p.ID, Reason: "retry now"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = f.store.Publication(t.Context(), p.ID)
	if p.Attempts != 0 || p.Status != model.PublicationPending {
		t.Fatal("explicit retry did not reset", p)
	}
	abandon, err := f.s.PreparePublicationAbandon(t.Context(), apicontract.Caller{Actor: admin()}, apicontract.PublicationAbandonArgs{ID: p.ID, Reason: "stop delivery"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = abandon(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = f.store.Publication(t.Context(), p.ID)
	if p.Status != model.PublicationAbandoned {
		t.Fatal(p)
	}
	if !strings.Contains(f.store.native.Audits()[len(f.store.native.Audits())-1].Details, "stop delivery") {
		t.Fatal("reason was not audited")
	}
}
func TestOlderResultNeverOverwrites(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	p := finishPublication(t, f, r)
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		return gitport.PublicationReceipt{}, &gitport.RefMovedError{Repository: repository, Ref: r.Ref}
	}
	if err := f.s.Deliver(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	p, _ = f.store.Publication(t.Context(), p.ID)
	r, _ = f.store.Run(t.Context(), r.ID)
	if p.Status != model.PublicationAbandoned || r.Status != stepmodel.RunPassed {
		t.Fatal("older result changed run or overwrote ref", p, r)
	}
}
func TestPublicationRejectsUnconfirmedReceipt(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		return gitport.PublicationReceipt{Repository: repository, ActionReceipt: p.ID, Refs: []gitport.Ref{{Name: p.Ref, ObjectSHA: strings.Repeat("d", 40)}}}, nil
	}
	if err := f.s.Deliver(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	p, _ = f.store.Publication(t.Context(), p.ID)
	if p.Status == model.PublicationDelivered {
		t.Fatal("unconfirmed object was called delivered")
	}
}
func TestPublicationRecoveryRequiresCacheConfirmation(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"confirmed", "cache pending", "bad receipt"} {
		t.Run(outcome, func(t *testing.T) { checkPublicationRecovery(t, outcome) })
	}
}
func checkPublicationRecovery(t *testing.T, outcome string) {
	t.Helper()
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	push := f.moving.Push
	f.moving.Push = func(ctx context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
		f.store.WriteError = errors.New("settlement unavailable")
		return push(ctx, p)
	}
	if err := f.s.Deliver(t.Context(), p.ID); err == nil {
		t.Fatal("settlement failure hidden")
	}
	f.store.WriteError = nil
	f.moving.Remote = func(context.Context, string, string) (gitport.Ref, error) {
		return gitport.Ref{Name: p.Ref, ObjectSHA: p.ObjectSHA, CommitSHA: *p.CommitSHA}, nil
	}
	confirmed := 0
	f.moving.Push = func(ctx context.Context, retry model.Publication) (gitport.PublicationReceipt, error) {
		confirmed++
		if retry.ID != p.ID || retry.ObjectSHA != p.ObjectSHA || retry.PreviousSHA == nil {
			t.Fatal("recovery changed the publication intent", retry)
		}
		if outcome == "cache pending" {
			return gitport.PublicationReceipt{}, &gitport.UnavailableError{Op: "cache"}
		}
		if outcome == "bad receipt" {
			return gitport.PublicationReceipt{}, nil
		}
		return push(ctx, retry)
	}
	if err := f.s.RecoverPublications(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = f.store.Publication(t.Context(), p.ID)
	want := map[string]model.PublicationStatus{"confirmed": model.PublicationDelivered, "cache pending": model.PublicationPending, "bad receipt": model.PublicationPending}[outcome]
	if confirmed != 1 || p.Status != want || outcome != "confirmed" && p.RetryAt == nil {
		t.Fatal("recovery skipped cache confirmation", p, confirmed)
	}
}
func TestPublicationLocksOnlyItsRefAndAbandonWaits(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		first := f.seed(stepmodel.RunRunning)
		p1 := finishPublication(t, f, first)
		second := first.Clone()
		second.ID = "bbbbbbbb000000000000000000000002"
		second.Ref = "refs/heads/other"
		p2 := finishPublication(t, f, second)
		started, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		push := f.moving.Push
		f.moving.Push = func(ctx context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
			if p.ID == p1.ID {
				close(started)
				<-release
			}
			return push(ctx, p)
		}
		done := make(chan error, 1)
		go func() { done <- f.s.Deliver(t.Context(), p1.ID) }()
		<-started
		if err := f.s.Deliver(t.Context(), p2.ID); err != nil {
			t.Fatal("another ref blocked", err)
		}
		call, err := f.s.PreparePublicationAbandon(t.Context(), apicontract.Caller{Actor: admin()}, apicontract.PublicationAbandonArgs{ID: p1.ID, Reason: "stop"})
		if err != nil {
			t.Fatal(err)
		}
		abandoned := make(chan error, 1)
		go func() { _, err := call(t.Context()); abandoned <- err }()
		synctest.Wait()
		var early bool
		select {
		case <-abandoned:
			early = true
		default:
		}
		unblock()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if early {
			t.Fatal("abandon did not wait")
		}
		requireCode(t, <-abandoned, publicerr.Conflict)
	})
}

// Release receipt unit boundary; the release inspection requires its real walk.
func TestReleaseReceiptOnlyAfterDeliveredPublication(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Ref = "refs/tags/v1.2.3"
	r.Origin.Trigger = model.TriggerTag
	r.Release = true
	r.ObjectSHA = strings.Repeat("c", 40)
	p := finishPublication(t, f, r)
	if _, ok := f.store.native.Release(p.ID); ok {
		t.Fatal("a passing run was called released")
	}
	f.moving.Remote = func(context.Context, string, string) (gitport.Ref, error) {
		return gitport.Ref{}, &gitport.AbsentError{Repository: repository, Ref: r.Ref}
	}
	if !f.timers.Fire() {
		t.Fatal("publication not armed")
	}
	workerOK(t, f)
	receipt, ok := f.store.native.Release(p.ID)
	if !ok || receipt.Tag != r.Ref || receipt.CommitSHA != head || receipt.RunID != r.ID {
		t.Fatal("exact release receipt missing", receipt)
	}
	checkReleaseObservation(t, f, receipt)
	p, _ = f.store.Publication(t.Context(), p.ID)
	if p.Release == nil || p.Status != model.PublicationDelivered {
		t.Fatal("public release claim lacks delivered receipt", p)
	}
}
func TestPublicationMutationFailuresKeepIntentAndNoPush(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	effects := 0
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		effects++
		return gitport.PublicationReceipt{}, nil
	}
	f.store.WriteError = errors.New("audit refused")
	if err := f.s.Deliver(t.Context(), p.ID); err == nil {
		t.Fatal("audit error hidden")
	}
	if effects != 0 {
		t.Fatal("push preceded committed claim")
	}
	f.store.WriteError = nil
	p, _ = f.store.Publication(t.Context(), p.ID)
	if p.Attempts != 0 {
		t.Fatal("failed claim advanced attempt", p)
	}
}

func exhaustPublication(t *testing.T, f *fixture, p model.Publication) model.Publication {
	t.Helper()
	for attempt := 1; attempt <= 5; attempt++ {
		if !f.timers.Fire() {
			t.Fatal("retry not armed", attempt)
		}
		workerOK(t, f)
		p, _ = f.store.Publication(t.Context(), p.ID)
		if p.Attempts != attempt {
			t.Fatal("attempt CAS", p)
		}
		if attempt < 5 {
			delay := (2 * time.Minute) << uint(attempt-1)
			if p.RetryAt == nil || p.RetryAt.Sub(f.c.Now()) != delay {
				t.Fatal("retry delay", p, delay)
			}
			f.offset.Add(int64(delay))
		} else if p.Status != model.PublicationFailed || p.RetryAt != nil || len(f.timers.Delays()) != 0 {
			t.Fatal("budget did not end", p)
		}
	}
	return p
}

func checkReleaseObservation(t *testing.T, f *fixture, receipt model.ReleaseReceipt) {
	t.Helper()
	page, err := f.store.Events(t.Context(), stateport.EventFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range page.Events {
		if e.Kind == string(model.EventReleasePublished) {
			var got model.ReleaseReceipt
			if json.Unmarshal(e.Data, &got) != nil || got != receipt {
				t.Fatal("release event mismatch", e)
			}
			found++
		}
	}
	if found != 1 {
		t.Fatal("release event not atomic", page)
	}
}

func TestInterruptedPublicationRearmsDurableBackoff(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	if _, err := f.store.StartPublicationAttempt(t.Context(), stateport.Action{Actor: caller().Identity}, stateport.PublicationAttempt{ID: p.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecoverPublications(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = f.store.Publication(t.Context(), p.ID)
	if p.RetryAt == nil || p.Attempts != 1 || p.Status != model.PublicationPending {
		t.Fatal("uncertain attempt was repeated or lost", p)
	}
}
func TestPublicationAbandonRequiresReasonAndDeliveredCannotBeAbandoned(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	c := apicontract.Caller{Actor: admin()}
	_, err := f.s.PreparePublicationAbandon(t.Context(), c, apicontract.PublicationAbandonArgs{ID: p.ID})
	requireField(t, err, "reason")
	if err := f.s.Deliver(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	call, err := f.s.PreparePublicationAbandon(t.Context(), c, apicontract.PublicationAbandonArgs{ID: p.ID, Reason: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = call(t.Context())
	requireCode(t, err, publicerr.Conflict)
}

func TestReceivedDeletionSchedulesOnlyCommittedFreshIntent(t *testing.T) {
	t.Parallel()
	f := setup(t)
	d := deletion()
	b := stateport.BatchAcceptance{ReceiveID: d.Request.ReceiveID, Repository: repository, Actor: caller(), Deletions: []stateport.BranchDeletion{d}}
	f.store.Err = errors.New("store unavailable")
	if _, err := f.s.Accept(t.Context(), b); err == nil || len(f.timers.Delays()) != 0 {
		t.Fatal("failed receive scheduled deletion", err)
	}
	f.store.Err = nil
	result, err := f.s.Accept(t.Context(), b)
	if err != nil || !result.Fresh || !f.timers.Fire() {
		t.Fatal("committed deletion was not scheduled", result, err)
	}
	workerOK(t, f)
	checkDeliveredDeletion(t, f, d.Publication.ID)
	result, err = f.s.Accept(t.Context(), b)
	if err != nil || result.Fresh || f.timers.Fire() {
		t.Fatal("receive replay repeated deletion", result, err)
	}
}

func checkDeliveredDeletion(t *testing.T, f *fixture, id string) {
	t.Helper()
	p, err := f.store.Publication(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != model.PublicationDelivered || p.ObjectSHA != "" || p.PreviousSHA == nil || *p.PreviousSHA != head {
		t.Fatal("deletion lost exact predecessor", p)
	}
}
