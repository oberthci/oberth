package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func seedStatusRun(t *testing.T, fixture *controlFixture, ref, sha, trigger string, kind model.RefKind, state model.RunStatus) model.Run {
	t.Helper()
	enqueued, err := fixture.store.EnqueueRun(t.Context(), model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: kind, Ref: ref, SHA: sha, Trigger: trigger, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.store.ClaimNextRun(t.Context())
	if err != nil || claimed.ID != enqueued.ID {
		t.Fatalf("claim = %s, %v", claimed.ID, err)
	}
	run, err := fixture.store.FinishRun(t.Context(), claimed.ID, model.RunResult{Status: state, TestedSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func statusHeadCall(t *testing.T, control *API, repo, ref string) (StatusResponse, error) {
	t.Helper()
	args, err := json.Marshal(map[string]string{"repo": repo, "ref": ref})
	if err != nil {
		t.Fatal(err)
	}
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "agent@host"}, "status", args)
	if err != nil {
		return StatusResponse{}, err
	}
	return value.(StatusResponse), nil
}

func TestStatusUsesCurrentBranchHead(t *testing.T) {
	const oldSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const unrelatedSHA = "cccccccccccccccccccccccccccccccccccccccc"
	cases := []struct {
		name, ref, trigger string
		old, current       model.RunStatus
	}{
		{"merged promotion", "promotion/main/bbbbbbbbbbbb", "promotion", model.RunFailed, model.RunPassed},
		{"fast-forward promotion", "feature/fix", "branch", model.RunFailed, model.RunPassed},
		{"current red cannot be hidden", "feature/fix", "branch", model.RunPassed, model.RunFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newControlFixture(t, JobResult{Status: model.RunPassed, Steps: []model.StepResult{stepResult(model.StepPassed)}})
			old := seedStatusRun(t, fixture, "main", oldSHA, "branch", model.RefBranch, tc.old)
			var current model.Run
			if tc.current == model.RunPassed {
				sourceSHA := headSHA
				if tc.trigger == "promotion" {
					sourceSHA = unrelatedSHA
				}
				seedGreenPromotionCandidate(t, fixture, sourceSHA)
				fixture.git.plan = gitcache.MergeCandidate{BaseSHA: oldSHA, MergedSHA: headSHA, FastForward: tc.trigger != "promotion"}
				value, err := fixture.api(t).CallTool(t.Context(), api.Actor{Identity: "agent@host", Admin: true}, "promote", json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
				if err != nil {
					t.Fatal(err)
				}
				promotion := requireToolPromotion(t, fixture, value)
				if promotion.RunID != "" {
					if err := fixture.scheduler.ProcessNext(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				current, err = fixture.store.ResolveRun(t.Context(), fixture.repo.ID, headSHA)
				if err != nil || current.Status != model.RunPassed {
					t.Fatalf("promotion proof = %#v, %v", current, err)
				}
			} else {
				seedStatusRun(t, fixture, tc.ref, headSHA, tc.trigger, model.RefBranch, model.RunPassed)
				current = seedStatusRun(t, fixture, "feature/recheck", headSHA, "branch", model.RefBranch, tc.current)
			}
			seedStatusRun(t, fixture, "other", unrelatedSHA, "branch", model.RefBranch, model.RunPassed)
			// A later historical main run must not override the current Git ref.
			seedStatusRun(t, fixture, "main", oldSHA, "branch", model.RefBranch, tc.old)
			fixture.refs = stubRefResolver{branches: map[string]map[string]string{"codeberg/acme/oberth": {"main": headSHA}}}
			control := fixture.api(t)
			renewals := &issueRenewFailer{Store: fixture.store, err: errors.New("status must not renew locks")}
			control.issues = renewals
			for _, repo := range []string{"", "oberth"} {
				for _, selector := range []string{"main", "refs/heads/main"} {
					got, err := statusHeadCall(t, control, repo, selector)
					if err != nil {
						t.Fatal(err)
					}
					if got.SHA != headSHA || got.RunID != current.ID || got.Ref != selector || got.Status != wireRunStatus(tc.current) {
						t.Errorf("status(%q,%q) = SHA %s, run %s, ref %s, status %s; want current %s", repo, selector, got.SHA, got.RunID, got.Ref, got.Status, current.ID)
					}
				}
			}
			if renewals.calls != 0 {
				t.Fatalf("status renewed %d locks", renewals.calls)
			}
			exact, err := statusHeadCall(t, control, "oberth", oldSHA)
			if err != nil || exact.SHA != old.SHA || exact.Status != wireRunStatus(tc.old) {
				t.Fatalf("historical SHA selector = %#v, %v", exact, err)
			}
		})
	}
}

func TestStatusCurrentHeadWithoutRunDoesNotReuseHistory(t *testing.T) {
	const oldSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixture := newControlFixture(t)
	seedStatusRun(t, fixture, "main", oldSHA, "branch", model.RefBranch, model.RunPassed)
	// A branch whose literal name equals HEAD's SHA is not exact-SHA evidence.
	seedStatusRun(t, fixture, headSHA, oldSHA, "branch", model.RefBranch, model.RunPassed)
	fixture.refs = stubRefResolver{branches: map[string]map[string]string{"codeberg/acme/oberth": {"main": headSHA}}}
	for _, repo := range []string{"", "oberth"} {
		got, err := statusHeadCall(t, fixture.api(t), repo, "main")
		if err != nil || got.Status != "no-runs" || got.SHA != headSHA || got.RunID != "" || len(got.Burns) != 0 {
			t.Fatalf("current untested HEAD = %#v, %v", got, err)
		}
	}
}

func TestStatusUnavailableBranchHeadDoesNotReuseHistory(t *testing.T) {
	fixture := newControlFixture(t)
	seedStatusRun(t, fixture, "main", strings.Repeat("a", 40), "branch", model.RefBranch, model.RunPassed)
	fixture.refs = stubRefResolver{}
	for _, repo := range []string{"", "oberth"} {
		if got, err := statusHeadCall(t, fixture.api(t), repo, "main"); err == nil {
			t.Fatalf("unavailable current ref returned historical evidence: %#v", got)
		}
	}
}

func TestStatusCurrentHeadPreservesSelectorAmbiguity(t *testing.T) {
	fixture := newControlFixture(t)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seedStatusRun(t, fixture, "main", sha, "branch", model.RefBranch, model.RunPassed)
	other, err := fixture.store.CreateRepository(t.Context(), model.RepositorySpec{Name: "other", UpstreamID: fixture.repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	fixture.refs = stubRefResolver{branches: map[string]map[string]string{
		"codeberg/acme/oberth": {"main": sha}, "codeberg/acme/" + other.Name: {"main": strings.Repeat("b", 40)},
	}}
	control := fixture.api(t)
	if _, err := statusHeadCall(t, control, "", "main"); !errors.Is(err, ErrAmbiguousRepository) {
		t.Fatalf("cross-repo selector = %v", err)
	}
	if got, err := statusHeadCall(t, control, "oberth", "main"); err != nil || got.SHA != sha {
		t.Fatalf("qualified selector = %#v, %v", got, err)
	}
	tag := seedStatusRun(t, fixture, "main", strings.Repeat("c", 40), "tag", model.RefTag, model.RunPassed)
	if _, err := statusHeadCall(t, control, "oberth", "main"); !errors.Is(err, store.ErrAmbiguous) {
		t.Fatalf("branch/tag ambiguity = %v", err)
	}
	if got, err := statusHeadCall(t, control, "oberth", "refs/tags/main"); err != nil || got.RunID != tag.ID {
		t.Fatalf("tag selector = %#v, %v", got, err)
	}
	if got, err := statusHeadCall(t, control, "oberth", "refs/heads/main"); err != nil || got.SHA != sha {
		t.Fatalf("branch selector = %#v, %v", got, err)
	}
}
