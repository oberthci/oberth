package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestQueuedStatusReasonMatchesAcrossSelectors(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	result, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, Ref: "feature/reason", RefKind: model.RefBranch, SHA: strings.Repeat("a", 40), Trigger: "push", Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	reader := f.api(t)
	direct, err := reader.statusForRun(ctx, f.repo, result.Run)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := reader.statusFromRun(ctx, f.repo, result.Run, "feature/reason", "agent@host")
	if err != nil {
		t.Fatal(err)
	}
	if selected.DisplayStatus != "queued" || selected.DisplayReason != direct.DisplayReason || selected.Run.DisplayReason != direct.DisplayReason {
		t.Fatalf("selector reason = %q / %q, want shared reason %q", selected.DisplayReason, selected.Run.DisplayReason, direct.DisplayReason)
	}
}
