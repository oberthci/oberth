package service_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestExplicitRetryRestoresReadBackMode(t *testing.T) {
	for _, provider := range []string{"fake-with-history", "fake-empty-history", "sqlite-empty-history"} {
		t.Run(provider, func(t *testing.T) { checkExplicitLandedRetry(t, provider) })
	}
}

func checkExplicitLandedRetry(t *testing.T, provider string) {
	t.Helper()
	f := explicitRetryService(t, provider)
	p := model.Publication{ID: "landed-retry", Repo: repository, Ref: "refs/heads/topic", Actor: caller().Identity, ObjectSHA: head, CommitSHA: new(head)}
	if err := f.c.Publications.PreparePublication(t.Context(), stateport.Action{Actor: p.Actor}, stateport.PublicationIntent{Publication: p}); err != nil {
		t.Fatal(err)
	}
	var modes []bool
	f.moving.Remote = func(context.Context, string, string) (gitport.Ref, error) {
		return gitport.Ref{}, &gitport.AbsentError{Ref: p.Ref}
	}
	push := f.moving.Push
	f.moving.Push = func(ctx context.Context, attempt model.Publication) (gitport.PublicationReceipt, error) {
		modes = append(modes, attempt.ReadBack)
		receipt, err := push(ctx, attempt)
		if err == nil && len(modes) == 1 {
			return gitport.PublicationReceipt{}, &gitport.UnavailableError{Op: "forge", Cause: errors.New("reply lost after a landed push")}
		}
		return receipt, err
	}
	if err := f.s.Deliver(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	deferred, err := f.c.Publications.Publication(t.Context(), p.ID)
	if err != nil || deferred.Status != model.PublicationPending || deferred.RetryAt == nil {
		t.Fatal("lost reply did not defer the landed publication", deferred, err)
	}
	retryLandedPublication(t, f, p.ID)
	if err := f.s.Deliver(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	checkExplicitRetryDelivered(t, f, p.ID, modes)
}

func explicitRetryService(t *testing.T, provider string) *fixture {
	t.Helper()
	f := setup(t)
	switch provider {
	case "sqlite-empty-history":
		state := landedStore(t, f)
		landedRepository(t, state)
		f.c.Publications = state
		f.restart(t)
	case "fake-with-history":
		f.seed(stepmodel.RunRunning)
	}
	return f
}

func checkExplicitRetryDelivered(t *testing.T, f *fixture, id string, modes []bool) {
	t.Helper()
	stored, err := f.c.Publications.Publication(t.Context(), id)
	if err != nil || stored.Status != model.PublicationDelivered || stored.Attempts != 1 || stored.ReadBack || !slices.Equal(modes, []bool{false, true}) {
		t.Fatal("explicit retry lost a landed publication", stored, modes, err)
	}
}

func retryLandedPublication(t *testing.T, f *fixture, id string) {
	t.Helper()
	call, err := f.s.PreparePublishRetry(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PublishRetryArgs{ID: id, Reason: "confirm the landed push"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t.Context()); err != nil {
		t.Fatal(err)
	}
}
