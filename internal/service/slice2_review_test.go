package service_test

import (
	"context"
	"strings"
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

func TestPromotionResultCommitIsExact(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Origin.Trigger = model.TriggerPromotion
	f.store.native.SeedRun(r)
	p := model.Promotion{ID: "exact-result", Repo: repository, Actor: caller().Identity, Caller: caller(), ResultSHA: head, RunID: r.ID, TargetRef: "refs/heads/main", BaseSHA: strings.Repeat("b", 40)}
	if e := f.store.PreparePromotion(t.Context(), stateport.Action{Actor: p.Actor}, p); e != nil {
		t.Fatal(e)
	}
	r.CommitSHA = new(strings.Repeat("c", 40))
	f.store.native.SeedRun(r)
	e := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunPassed})
	if e != nil {
		t.Fatal(e)
	}
	stored, e := f.store.Run(t.Context(), r.ID)
	if e != nil || stored.Status != stepmodel.RunFailed || stored.DisplayReason != "promotion evidence invalid" {
		t.Fatal("invalid promotion did not finish safely", stored, e)
	}
	if len(f.store.native.PublicationValues()) != 0 {
		t.Fatal("untested result acquired publication")
	}
}
func TestOnlyReleaseRunsAcquireReleaseReceipt(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"refs/heads/topic", "refs/tags/ordinary"} {
		t.Run(ref, func(t *testing.T) {
			f := setup(t)
			r := f.seed(stepmodel.RunRunning)
			r.Ref = ref
			r.Release = false
			p := finishPublication(t, f, r)
			if e := f.s.Deliver(t.Context(), p.ID); e != nil {
				t.Fatal(e)
			}
			got, e := f.store.Publication(t.Context(), p.ID)
			if e != nil || got.Status != model.PublicationDelivered || got.Release != nil {
				t.Fatal("non-release settlement lied", got, e)
			}
			page, e := f.store.Events(t.Context(), stateport.EventFilter{Repositories: []string{repository}, Limit: 200})
			if e != nil || countEventKind(page, model.EventReleasePublished) != 0 {
				t.Fatal("non-release emitted release event", page, e)
			}
		})
	}
}
func TestSyncRejectsEveryNonHeadNamespace(t *testing.T) {
	t.Parallel()
	f := setup(t)
	for _, ref := range []string{"refs/tags/ordinary", "refs/notes/review", "refs/remotes/origin/topic"} {
		_, e := f.s.PrepareSync(t.Context(), workerCaller(), apicontract.SyncArgs{Repo: repository, Branch: ref, SHA: head})
		requireCode(t, e, publicerr.Forbidden)
	}
	if len(f.store.native.PublicationValues()) != 0 {
		t.Fatal("non-head sync recorded intent")
	}
}
func TestAnnotatedTagReadbackRequiresObjectIdentity(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Ref = "refs/tags/ordinary"
	r.ObjectSHA = strings.Repeat("c", 40)
	r.Origin.Trigger = model.TriggerTag
	p := finishPublication(t, f, r)
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		return gitport.PublicationReceipt{Repository: repository, ActionReceipt: p.ID, Refs: []gitport.Ref{{Name: p.Ref, ObjectSHA: strings.Repeat("d", 40), CommitSHA: head}}}, nil
	}
	if e := f.s.Deliver(t.Context(), p.ID); e != nil {
		t.Fatal(e)
	}
	got, e := f.store.Publication(t.Context(), p.ID)
	if e != nil || got.Status == model.PublicationDelivered {
		t.Fatal("commit identity substituted for annotated object", got, e)
	}
}
func TestPromotionSuccessorRetainsOriginalAuthority(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	out := promote(t, f)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: head, CommitSHA: head}, gitport.File{Bytes: []byte("uplinks_may_promote: false\n")}, nil
	}
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		return gitport.PublicationReceipt{}, &gitport.RefMovedError{Repository: repository, Ref: "refs/heads/main"}
	}
	e := f.s.Deliver(t.Context(), "publication:"+out.ID)
	requireCode(t, e, publicerr.Forbidden)
	rows, e := f.store.Promotions(t.Context(), stateport.RecordFilter{Limit: 10})
	if e != nil || len(rows) != 1 {
		t.Fatal("forbidden successor created work", rows, e)
	}
}
func TestTagPublicationIsCreationOnly(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Ref = "refs/tags/v1.2.3"
	r.Release = true
	p := finishPublication(t, f, r)
	reads := 0
	f.moving.Remote = func(context.Context, string, string) (gitport.Ref, error) {
		reads++
		return gitport.Ref{Name: r.Ref, ObjectSHA: strings.Repeat("d", 40), CommitSHA: head}, nil
	}
	f.moving.Push = func(_ context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
		if p.PreviousSHA == nil || *p.PreviousSHA != "" {
			t.Fatal("tag replacement permitted", p)
		}
		return gitport.PublicationReceipt{}, &gitport.RefMovedError{Ref: p.Ref}
	}
	if e := f.s.Deliver(t.Context(), p.ID); e != nil {
		t.Fatal(e)
	}
	got, e := f.store.Publication(t.Context(), p.ID)
	if e != nil || got.Status != model.PublicationAbandoned || reads != 0 {
		t.Fatal("tag looked up replacement base", got, reads, e)
	}
}
func TestPromotionEvidenceSelectsBranchTrigger(t *testing.T) {
	t.Parallel()
	f := setup(t)
	branch := f.seed(stepmodel.RunFailed)
	other := branch.Clone()
	other.ID = "deadbeef000000000000000000000001"
	other.Origin.Trigger = model.TriggerTag
	other.Status = stepmodel.RunPassed
	other.Sequence = 20
	f.store.native.SeedRun(other)
	_, e := f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, e, publicerr.Forbidden)
}
func TestArchivedPromoteAndSyncRefuse(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	f.archiveRepository(t, repository)
	_, e := f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, e, publicerr.InvalidTransition)
	_, e = f.s.PrepareSync(t.Context(), workerCaller(), apicontract.SyncArgs{Repo: repository, Branch: "topic", SHA: head})
	requireCode(t, e, publicerr.InvalidTransition)
}
func TestPromotionDefaultHeadMustBeBranch(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{Name: "refs/tags/default", ObjectSHA: head, CommitSHA: head}, gitport.File{Bytes: []byte("uplinks_may_promote: true\n")}, nil
	}
	_, e := f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, e, publicerr.InvalidInput)
}
func TestPolicyOutageRetainsUnavailableClass(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{}, gitport.File{}, &gitport.UnavailableError{Op: "policy read"}
	}
	_, e := f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, e, publicerr.Unavailable)
}
func TestPublicationHonorsDueTimeAndSixthAttemptRefuses(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		return gitport.PublicationReceipt{}, &gitport.UnavailableError{Op: "push"}
	}
	if e := f.s.Deliver(t.Context(), p.ID); e != nil {
		t.Fatal(e)
	}
	before, _ := f.store.Publication(t.Context(), p.ID)
	if e := f.s.Deliver(t.Context(), p.ID); e != nil {
		t.Fatal(e)
	}
	after, _ := f.store.Publication(t.Context(), p.ID)
	if before.Attempts != after.Attempts {
		t.Fatal("future retry attempted early")
	}
	p = exhaustPublication(t, f, after)
	if p.Attempts != 5 || p.Status != model.PublicationFailed {
		t.Fatal("wrong retry budget", p)
	}
	page, e := f.store.Events(t.Context(), stateport.EventFilter{Repositories: []string{repository}, Limit: 200})
	if e != nil || countEventKind(page, model.EventPublicationFailed) != 1 {
		t.Fatal("retry budget omitted terminal event", page, e)
	}
	if _, e := f.store.StartPublicationAttempt(t.Context(), stateport.Action{Actor: p.Actor}, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: 5}); e == nil {
		t.Fatal("sixth attempt accepted")
	}
}

func TestSuccessorOutlivesDeliveryLockAndDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.seed(stepmodel.RunPassed)
		out := promote(t, f)
		entered := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 1)
		f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
			return gitport.PublicationReceipt{}, &gitport.RefMovedError{Ref: "refs/heads/main"}
		}
		f.moving.Prepare = func(_ context.Context, q gitport.PromotionRequest) (gitport.Promotion, error) {
			return gitport.Promotion{Result: gitport.Ref{Name: q.Target.Name, ObjectSHA: strings.Repeat("c", 40), CommitSHA: strings.Repeat("c", 40)}}, nil
		}
		f.starter.Start = func(ctx context.Context, _ service.StartRequest) (apicontract.Receipt, error) {
			close(entered)
			<-release
			timer := time.NewTimer(31 * time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return apicontract.Receipt{}, ctx.Err()
			case <-timer.C:
				return apicontract.Receipt{ID: "successor-child"}, nil
			}
		}
		go func() { done <- f.s.Deliver(t.Context(), "publication:"+out.ID) }()
		receive(t, entered)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		e := f.s.Deliver(ctx, "publication:"+out.ID)
		cancel()
		close(release)
		if e != nil {
			t.Fatal("target lock held through successor", e)
		}
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		select {
		case e = <-done:
			if e != nil {
				t.Fatal("delivery deadline cut successor", e)
			}
		case <-timer.C:
			t.Fatal("successor did not complete")
		}
	})
}

func countEventKind(p model.EventPage, kind model.EventKind) int {
	n := 0
	for _, e := range p.Events {
		if e.Kind == string(kind) {
			n++
		}
	}
	return n
}
