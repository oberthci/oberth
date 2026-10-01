package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestResolveWaitRunPreservesIdentityAndTriggerBoundaries(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	database := testStore(t, &now)
	repo := createRepo(t, database)
	commitSHA := strings.Repeat("a", 40)
	enqueue := func(ref, sha, tested, trigger string, kind model.RefKind) model.Run {
		t.Helper()
		run, err := database.EnqueueRun(ctx, model.RunSpec{
			RepoID: repo.ID, RefKind: kind, Ref: ref, SHA: sha, TestedSHA: tested,
			Trigger: trigger, Release: kind == model.RefTag, Actor: "agent@host",
		})
		if err != nil {
			t.Fatal(err)
		}
		return run.Run
	}
	first := enqueue("v1.0.0", strings.Repeat("b", 40), commitSHA, "tag", model.RefTag)
	second := enqueue("v1.0.1", strings.Repeat("c", 40), commitSHA, "tag", model.RefTag)
	branch := enqueue("feature/next", commitSHA, commitSHA, "branch", model.RefBranch)
	enqueue("v2.0.0", strings.Repeat("d", 40), strings.Repeat("e", 40), "tag", model.RefTag)
	for _, test := range []struct{ name, selector, trigger, want string }{
		{"exact commit selects newest tag", commitSHA, "tag", second.ID},
		{"short commit with multiple tags", commitSHA[:7], "tag", second.ID},
		{"exact object selects its own tag", first.SHA, "tag", first.ID},
		{"short object", first.SHA[:7], "tag", first.ID},
		{"branch trigger", commitSHA, "branch", branch.ID},
		{"unfiltered uses newest matching run", commitSHA, "", branch.ID},
	} {
		t.Run(test.name, func(t *testing.T) {
			run, err := database.ResolveWaitRun(ctx, repo.ID, test.selector, test.trigger)
			if err != nil || run.ID != test.want {
				t.Fatalf("wait resolution = %#v, %v; want %s", run, err, test.want)
			}
		})
	}
	// Ordinary status/promotion resolution must retain object-SHA semantics.
	if run, err := database.ResolveRun(ctx, repo.ID, commitSHA); err != nil || run.ID != branch.ID {
		t.Fatalf("generic resolver changed: %#v, %v", run, err)
	}
	enqueue("other-tested-commit", strings.Repeat("f", 40), strings.Repeat("9", 40), "branch", model.RefBranch)
	if _, err := database.ResolveWaitRun(ctx, repo.ID, strings.Repeat("9", 40), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-tag TestedSHA became an alias: %v", err)
	}
	// The collision is between an object identity and a peeled commit, not
	// simply two run rows sharing a commit. Exact selectors remain usable.
	enqueue("v3.0.0", "aaaaaaa"+strings.Repeat("f", 33), strings.Repeat("8", 40), "tag", model.RefTag)
	if _, err := database.ResolveWaitRun(ctx, repo.ID, "aaaaaaa", "tag"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("object/commit prefix collision = %v, want ambiguous", err)
	}
	if run, err := database.ResolveWaitRun(ctx, repo.ID, commitSHA, "tag"); err != nil || run.ID != second.ID {
		t.Fatalf("exact commit after prefix collision = %#v, %v", run, err)
	}
	if _, err := database.ResolveWaitRun(ctx, repo.ID, first.SHA, "branch"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("opposite trigger should not match: %v", err)
	}
	if _, err := database.ResolveWaitRun(ctx, repo.ID+1, first.SHA, "tag"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("run crossed repository boundary: %v", err)
	}
}
