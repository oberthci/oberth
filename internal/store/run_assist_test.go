package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// TestAssistedRunIsNeverGreenEvidence proves the durable side of assisted
// continuation: the opt-in is stored with the run, a continue marks the run
// assisted atomically with its audit row, the verdict is monotonic, and a
// passed assisted run no longer satisfies the SHA-pinned fragment gate.
func TestAssistedRunIsNeverGreenEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "fragment", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 40)
	plain, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repository.ID, RefKind: model.RefBranch, Ref: "feature/plain", SHA: sha, TestedSHA: sha, Actor: "tester", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.AssistRequested || plain.Assisted || plain.Assist != nil {
		t.Fatalf("plain run carries assist state: %+v", plain)
	}
	if _, err := s.AppendRunAssist(ctx, model.RunAssistRequest{RunID: plain.ID, Event: model.RunAssistEvent{Actor: "tester", Step: "test", Kind: model.RunAssistContinue}}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("hold action on a run that never asked: %v", err)
	}
	if _, err := s.FinishRun(ctx, plain.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}

	sha = strings.Repeat("c", 40)
	run, err := s.EnqueueRun(ctx, model.RunSpec{Assist: true, RepoID: repository.ID, RefKind: model.RefBranch, Ref: "feature/assist", SHA: sha, TestedSHA: sha, Actor: "tester", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if !run.AssistRequested || run.Assisted {
		t.Fatalf("assist request lost or verdict premature: %+v", run)
	}
	if _, err := s.AppendRunAssist(ctx, model.RunAssistRequest{RunID: run.ID, Event: model.RunAssistEvent{Actor: "tester", Step: "test", Kind: "inspect"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown action accepted: %v", err)
	}
	refused := errors.New("not yours")
	if _, err := s.AppendRunAssist(ctx, model.RunAssistRequest{RunID: run.ID, Authorize: func(model.Run) error { return refused }, Event: model.RunAssistEvent{Actor: "other", Step: "test", Kind: model.RunAssistExec}}); !errors.Is(err, refused) {
		t.Fatalf("authorize ignored: %v", err)
	}
	exitCode := 1
	updated, err := s.AppendRunAssist(ctx, model.RunAssistRequest{RunID: run.ID, Event: model.RunAssistEvent{Actor: "tester", Step: "test", Pod: "wf-test-1", Kind: model.RunAssistExec, Argv: []string{"go", "test", "./..."}, ExitCode: &exitCode}})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Assisted || updated.Assist == nil || len(updated.Assist.Events) != 1 {
		t.Fatalf("exec alone must mark the run assisted (an exec can edit the tree or release the hold): %+v", updated)
	}
	if reason := updated.Assist.Reason(); reason != "assisted: step test inspected by tester; not evidence" {
		t.Fatalf("exec-only reason = %q", reason)
	}
	pods, err := s.AssistPodActivity(ctx)
	if err != nil || len(pods) != 1 || pods["wf-test-1"].Released || pods["wf-test-1"].LastAction.IsZero() {
		t.Fatalf("exec released a hold or left no activity: %v, %v", pods, err)
	}
	updated, err = s.AppendRunAssist(ctx, model.RunAssistRequest{RunID: run.ID, Event: model.RunAssistEvent{Actor: "tester", Step: "test", Pod: "wf-test-1", Kind: model.RunAssistPatch, PatchSHA256: strings.Repeat("d", 64), PatchBytes: 120, Files: []string{"go.mod"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Assisted || !strings.Contains(updated.Assist.Reason(), "step test patched sha256:dddddddddddd by tester") {
		t.Fatalf("patch alone must mark the run assisted: %+v %q", updated.Assisted, updated.Assist.Reason())
	}
	updated, err = s.AppendRunAssist(ctx, model.RunAssistRequest{RunID: run.ID, Event: model.RunAssistEvent{Actor: "tester", Step: "test", Pod: "wf-test-1", Kind: model.RunAssistContinue}})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Assisted || !updated.Assist.Continued() || !updated.Assist.Events[2].Assisted {
		t.Fatalf("continue after patch did not mark the run assisted: %+v", updated.Assist)
	}
	if reason := updated.Assist.Reason(); !strings.Contains(reason, "step test continued after patch sha256:dddddddddddd by tester") || !strings.HasSuffix(reason, "not evidence") {
		t.Fatalf("reason line = %q", reason)
	}
	if pods, err = s.AssistPodActivity(ctx); err != nil || !pods["wf-test-1"].Released || len(pods) != 1 {
		t.Fatalf("released pods after continue = %v, %v", pods, err)
	}
	if _, err := s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	finished, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != model.RunPassed || !finished.Assisted {
		t.Fatalf("finished assisted run = %s assisted=%v", finished.Status, finished.Assisted)
	}
	passed, err := s.HasPassedRunForSHA(ctx, "acme/fragment", sha)
	if err != nil || passed {
		t.Fatalf("assisted passed run counted as green for its SHA: %v, %v", passed, err)
	}
	if passed, err := s.HasPassedRunForSHA(ctx, "acme/fragment", strings.Repeat("b", 40)); err != nil || !passed {
		t.Fatalf("plain passed run lost: %v, %v", passed, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET assisted = 0 WHERE id = ?`, run.ID); err == nil {
		t.Fatal("assisted verdict could be cleared")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET assist_requested = 0 WHERE id = ?`, run.ID); err == nil {
		t.Fatal("assist request could be rewritten")
	}
	var audited int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE resource_id = ? AND action LIKE 'run.assist.%'`, run.ID).Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("assist audit rows = %d, %v", audited, err)
	}

	// A patch with no exec before it is enough on its own.
	sha = strings.Repeat("f", 40)
	patched, err := s.EnqueueRun(ctx, model.RunSpec{Assist: true, RepoID: repository.ID, RefKind: model.RefBranch, Ref: "feature/patched", SHA: sha, TestedSHA: sha, Actor: "tester", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if patchedRun, err := s.AppendRunAssist(ctx, model.RunAssistRequest{RunID: patched.ID, Event: model.RunAssistEvent{Actor: "tester", Step: "lint", Pod: "wf-lint-1", Kind: model.RunAssistPatch, PatchSHA256: strings.Repeat("1", 64)}}); err != nil || !patchedRun.Assisted {
		t.Fatalf("patch alone = %+v, %v", patchedRun.Assisted, err)
	}
	if pods, err := s.AssistPodActivity(ctx); err != nil || pods["wf-lint-1"].Released {
		t.Fatalf("patch released a hold: %v, %v", pods, err)
	}

	// A rerun carries the caller's choice, not the source run's.
	sha = strings.Repeat("e", 40)
	red, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repository.ID, RefKind: model.RefBranch, Ref: "feature/red", SHA: sha, TestedSHA: sha, Actor: "tester", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, red.ID, model.RunResult{Status: model.RunFailed, FailedStep: "test", Error: "step test failed"}); err != nil {
		t.Fatal(err)
	}
	rerun, err := s.RerunRun(ctx, model.RunRerunRequest{RunID: red.ID, Actor: "tester", Reason: "again", Assist: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rerun.Enqueued.AssistRequested || rerun.Enqueued.Assisted {
		t.Fatalf("rerun assist state = requested %v assisted %v", rerun.Enqueued.AssistRequested, rerun.Enqueued.Assisted)
	}
}

// TestSweepAbandonIsRecordedAndAudited proves a server sweep abandon lands in
// the run's assist record and audit with its reason and times, leaves the
// run unassisted (no agent touched it), releases the Pod for the next sweep,
// and produces the abandoned-hold reason line.
func TestSweepAbandonIsRecordedAndAudited(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "fragment", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	run, err := s.EnqueueRun(ctx, model.RunSpec{Assist: true, RepoID: repository.ID, RefKind: model.RefBranch, Ref: "feature/held", SHA: sha, TestedSHA: sha, Actor: "tester", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	exit := 1
	held := now.Add(-20 * time.Minute)
	abandon := model.AssistAbandon{RunID: run.ID, Pod: "wf-test-1", Step: "test", Reason: model.AssistAbandonTTL, HeldSince: held, AbandonedAt: now, FailedExitCode: &exit}
	updated, err := s.AppendRunAssist(ctx, model.RunAssistRequest{RunID: run.ID, Event: abandon.Event()})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Assisted {
		t.Fatal("an unserviced sweep abandon marked the run assisted")
	}
	if !updated.Assist.SweepAbandoned("test") || updated.Assist.SweepAbandoned("lint") {
		t.Fatalf("sweep abandon not attributed to its step: %+v", updated.Assist)
	}
	want := "assisted: step test failed at attempt 0 (exit 1); hold abandoned unserviced after 20m0s (ttl); not evidence"
	if reason := updated.Assist.Reason(); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
	if pods, err := s.AssistPodActivity(ctx); err != nil || !pods["wf-test-1"].Released || !pods["wf-test-1"].LastAction.IsZero() {
		t.Fatalf("abandoned pod activity = %v, %v", pods, err)
	}
	var actor, details string
	if err := s.db.QueryRowContext(ctx, `SELECT actor, details FROM audit_actions WHERE resource_id = ? AND action = 'run.assist.abandon'`, run.ID).Scan(&actor, &details); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"reason":"ttl"`, `"step":"test"`, `"pod":"wf-test-1"`, `"held_since":"2026-11-08T23:40:00Z"`, `"abandoned_at":"2026-11-09T00:00:00Z"`} {
		if !strings.Contains(details, fragment) {
			t.Fatalf("audit details %s lack %s", details, fragment)
		}
	}
	if actor != "oberth" {
		t.Fatalf("audit actor = %q", actor)
	}
}
