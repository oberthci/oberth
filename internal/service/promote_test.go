package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func promote(t *testing.T, f *fixture) apicontract.PromoteResult {
	t.Helper()
	call, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head, Reason: "land tested work"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Covers: REQ-C2-006
func TestExactTestedPromotionContract(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunPassed)
	r.Origin.AssistRequested = true
	f.store.native.SeedRun(r)
	starts := 0
	f.starter.Start = func(context.Context, service.StartRequest) (apicontract.Receipt, error) {
		starts++
		return apicontract.Receipt{}, errors.New("unnecessary run")
	}
	out := promote(t, f)
	if out.TestedRunID != r.ID || out.CommitSHA != head || out.RunID != r.ID || out.ID == "" || starts != 0 {
		t.Fatal("promotion lost exact passing evidence", out, starts)
	}
	checkFastForwardRecord(t, f, out)
}
func checkFastForwardRecord(t *testing.T, f *fixture, out apicontract.PromoteResult) {
	t.Helper()
	p, err := f.store.Promotion(t.Context(), out.ID)
	if err != nil || p.SourceSHA != head || p.ResultSHA != head || p.BaseSHA != strings.Repeat("b", 40) || p.Status != model.PromotionPassed {
		t.Fatal("wrong exact/base binding", p, err)
	}
	intent, err := f.store.Publication(t.Context(), "publication:"+p.ID)
	if err != nil || intent.ObjectSHA != head || intent.PreviousSHA == nil || *intent.PreviousSHA != p.BaseSHA {
		t.Fatal("publication not atomic with passed evidence", intent, err)
	}
	checkFastForwardDelivery(t, f, intent)
}
func checkFastForwardDelivery(t *testing.T, f *fixture, intent model.Publication) {
	t.Helper()
	if !f.timers.Fire() {
		t.Fatal("publication not armed")
	}
	workerOK(t, f)
	intent, _ = f.store.Publication(t.Context(), intent.ID)
	if intent.Status != model.PublicationDelivered {
		t.Fatal("tested commit not delivered", intent)
	}
	for _, a := range f.store.native.Audits() {
		if a.Action == "promotion.prepare" && !strings.Contains(a.Details, "land tested work") {
			t.Fatal("reason not audited", a)
		}
	}
}

func TestPromoteMergedResultIsTested(t *testing.T) {
	t.Parallel()
	for _, status := range []stepmodel.RunStatus{stepmodel.RunPassed, stepmodel.RunFailed} {
		t.Run(string(status), func(t *testing.T) { checkMergedPromotion(t, status) })
	}
}
func checkMergedPromotion(t *testing.T, status stepmodel.RunStatus) {
	t.Helper()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	merged := strings.Repeat("d", 40)
	f.moving.Prepare = func(_ context.Context, q gitport.PromotionRequest) (gitport.Promotion, error) {
		return gitport.Promotion{Result: gitport.Ref{Name: q.Target.Name, ObjectSHA: merged, CommitSHA: merged}}, nil
	}
	out := promote(t, f)
	if out.RunID == runID || out.CommitSHA != merged || len(f.store.native.PublicationValues()) != 0 {
		t.Fatal("untested result was published", out)
	}
	r, err := f.store.Run(t.Context(), out.RunID)
	if err != nil || r.ObjectSHA != merged || r.CommitSHA == nil || *r.CommitSHA != merged || r.Origin.Trigger != model.TriggerPromotion {
		t.Fatal("wiring did not receive exact result", r, err)
	}
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: status}); err != nil {
		t.Fatal(err)
	}
	checkMergedVerdict(t, f, out.ID, status)
}
func checkMergedVerdict(t *testing.T, f *fixture, id string, status stepmodel.RunStatus) {
	t.Helper()
	p, _ := f.store.Promotion(t.Context(), id)
	if status == stepmodel.RunFailed {
		if p.Status != model.PromotionFailed || len(f.store.native.PublicationValues()) != 0 {
			t.Fatal("failed promotion published", p)
		}
		return
	}
	if p.Status != model.PromotionPassed || len(f.store.native.PublicationValues()) != 1 {
		t.Fatal("run/promotion/intent not committed together", p)
	}
	if !f.timers.Fire() {
		t.Fatal("publication not armed")
	}
	workerOK(t, f)
}

func TestPromoteRefusesRedUntestedAndUnreadPolicy(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"red", "assisted", "untested", "policy outage", "invalid policy", "denied policy", "wrong target"} {
		t.Run(which, func(t *testing.T) {
			f := setup(t)
			r := f.seed(stepmodel.RunPassed)
			q := apicontract.PromoteArgs{Repo: repository, SHA: head}
			switch which {
			case "red":
				r.Status = stepmodel.RunFailed
				f.store.native.SeedRun(r)
			case "assisted":
				r.Origin.Assisted = true
				f.store.native.SeedRun(r)
			case "untested":
				q.SHA = strings.Repeat("f", 40)
			case "policy outage":
				f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
					return gitport.Ref{}, gitport.File{}, &gitport.UnavailableError{Op: "policy"}
				}
			case "invalid policy":
				f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
					return gitport.Ref{Name: "refs/heads/main", ObjectSHA: head, CommitSHA: head}, gitport.File{Bytes: []byte("uplinks_may_promote: invalid")}, nil
				}
			case "denied policy":
				f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
					return gitport.Ref{Name: "refs/heads/main", ObjectSHA: head, CommitSHA: head}, gitport.File{Bytes: []byte("uplinks_may_promote: false")}, nil
				}
			case "wrong target":
				q.Branch = "other"
			}
			if call, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, q); err == nil || call != nil {
				t.Fatal("unsafe promotion prepared")
			}
			if len(f.store.native.PublicationValues()) != 0 {
				t.Fatal("refused promotion wrote an intent")
			}
		})
	}
}
func TestPromotionReadsPolicyFreshAndKnownRedCannotSkip(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunPassed)
	reads := 0
	allowed := true
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		reads++
		body := "uplinks_may_promote: false\nallow_without_branch_ci: true"
		if allowed {
			body = "uplinks_may_promote: true\nallow_without_branch_ci: true"
		}
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("b", 40), CommitSHA: strings.Repeat("b", 40)}, gitport.File{Bytes: []byte(body)}, nil
	}
	promote(t, f)
	allowed = false
	if _, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head}); err == nil {
		t.Fatal("cached policy authorized promotion")
	}
	allowed = true
	r.Status = stepmodel.RunFailed
	f.store.native.SeedRun(r)
	if _, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head}); err == nil {
		t.Fatal("skip policy accepted known red")
	}
	if reads != 3 {
		t.Fatal("policy was not fresh", reads)
	}
}
func TestMissingPolicyIsNotAnUpstreamOutage(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		base := strings.Repeat("b", 40)
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: base, CommitSHA: base}, gitport.File{}, &gitport.AbsentError{Repository: repository, Path: ".oberth/promotion.yaml"}
	}
	if _, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: admin()}, apicontract.PromoteArgs{Repo: repository, SHA: head}); err != nil {
		t.Fatal("operator could not promote without optional policy", err)
	}
	_, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, err, publicerr.Forbidden)
}
func TestPromoteMovedTargetTwice(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	preparations := 0
	currentHead := strings.Repeat("b", 40)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: currentHead, CommitSHA: currentHead}, gitport.File{Bytes: []byte("uplinks_may_promote: true\n")}, nil
	}
	f.moving.Prepare = func(_ context.Context, q gitport.PromotionRequest) (gitport.Promotion, error) {
		if q.Target.ObjectSHA != currentHead {
			t.Fatal("successor used stale default head", q.Target, currentHead)
		}
		preparations++
		sha := strings.Repeat(string(rune('c'+preparations)), 40)
		return gitport.Promotion{Result: gitport.Ref{Name: q.Target.Name, ObjectSHA: sha, CommitSHA: sha}}, nil
	}
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		currentHead = strings.Repeat(string(rune('a'+preparations)), 40)
		return gitport.PublicationReceipt{}, &gitport.RefMovedError{Repository: repository, Ref: "refs/heads/main"}
	}
	out := promote(t, f)
	id := out.ID
	for successor := 0; successor <= 2; successor++ {
		p, err := f.store.Promotion(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Successors != successor {
			t.Fatal("successor lineage lost", p)
		}
		rows := completeMovedPromotion(t, f, p)
		if successor == 2 {
			p, _ = f.store.Promotion(t.Context(), id)
			if p.Status != model.PromotionFailed || p.Reason != "default branch moved; promote again" || preparations != 3 {
				t.Fatal("third movement started more work", p, preparations)
			}
			break
		}
		id = nextPromotion(t, rows, id)

	}
}
func TestAssistedRunIsNoEvidence(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Origin.AssistRequested = true
	f.store.native.SeedRun(r)
	if err := f.store.Assist(t.Context(), stateport.Action{Actor: caller().Identity}, stateport.HoldAction{RunID: r.ID, Step: "build", NodeID: "node", PodUID: "pod", Kind: "exec", Revision: r.Revision, Argv: []string{"true"}, TimeoutS: 60}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	if len(f.store.native.PublicationValues()) != 0 || len(f.timers.Delays()) != 0 {
		t.Fatal("assisted pass became publication evidence")
	}
	_, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, err, publicerr.Forbidden)
}

// Covers: REQ-C2-015
func TestNoPromotionBypassThroughSyncContract(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"default", "internal ref", "promotion run", "release run", "assisted", "prefix", "wrong object", "hidden"} {
		t.Run(which, func(t *testing.T) {
			f := setup(t)
			q := apicontract.SyncArgs{Repo: repository, Branch: "topic", SHA: head}
			switch which {
			case "default":
				q.Branch = "main"
			case "internal ref":
				q.Branch = "refs/oberth/promotion/example"
			case "promotion run":
				r := f.seed(stepmodel.RunPassed)
				r.Origin.Trigger = model.TriggerPromotion
				f.store.native.SeedRun(r)
			case "release run":
				r := f.seed(stepmodel.RunPassed)
				r.Release = true
				f.store.native.SeedRun(r)
			case "assisted":
				r := f.seed(stepmodel.RunPassed)
				r.Origin.Assisted = true
				f.store.native.SeedRun(r)
			case "prefix":
				q.SHA = head[:8]
			case "wrong object":
				q.SHA = strings.Repeat("c", 40)
			case "hidden":
				q.Repo = "private/team/hidden"
			}
			if call, err := f.s.PrepareSync(t.Context(), apicontract.Caller{Actor: caller()}, q); err == nil || call != nil {
				t.Fatal("sync bypass prepared")
			}
		})
	}
}
func TestSyncParkedBranchDoesNotRun(t *testing.T) {
	t.Parallel()
	f := setup(t)
	starts := 0
	f.starter.Start = func(context.Context, service.StartRequest) (apicontract.Receipt, error) {
		starts++
		return apicontract.Receipt{}, errors.New("unexpected run")
	}
	call, err := f.s.PrepareSync(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.SyncArgs{Repo: repository, Branch: "topic", SHA: head, Reason: "publish parked branch"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if starts != 0 || out.ID == "" {
		t.Fatal("parked sync ran work")
	}
	if !f.timers.Fire() {
		t.Fatal("sync was not armed")
	}
	workerOK(t, f)
	p, _ := f.store.Publication(t.Context(), out.ID)
	if p.Status != model.PublicationDelivered || p.RunID != "" || p.ObjectSHA != head {
		t.Fatal("wrong parked publication", p)
	}
}

func completeMovedPromotion(t *testing.T, f *fixture, p model.Promotion) []model.Promotion {
	t.Helper()
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: p.RunID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Deliver(t.Context(), "publication:"+p.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := f.store.Promotions(t.Context(), stateport.RecordFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func nextPromotion(t *testing.T, rows []model.Promotion, id string) string {
	t.Helper()
	for _, p := range rows {
		if p.PredecessorID == id {
			return p.ID
		}
	}
	t.Fatal("successor not recorded")
	return ""
}

func TestPromotionConflictsAndInvalidFreshHeadRefuse(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	f.moving.Prepare = func(context.Context, gitport.PromotionRequest) (gitport.Promotion, error) {
		return gitport.Promotion{Conflicts: []string{"pipeline.yaml"}}, nil
	}
	call, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head})
	if err != nil {
		t.Fatal(err)
	}
	_, err = call(t.Context())
	requireCode(t, err, publicerr.Conflict)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{}, gitport.File{}, &gitport.AbsentError{Path: ".oberth/promotion.yaml"}
	}
	_, err = f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: admin()}, apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, err, publicerr.InvalidInput)
}
