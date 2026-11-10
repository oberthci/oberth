package store_test

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestActiveRunIDsListsEveryNonTerminalRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	database, err := storetest.Open(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, ref := range []string{"a", "b", "c"} {
		run, err := storetest.EnqueueRun(database, ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: ref,
			SHA: "dddddddddddddddddddddddddddddddddddddddd", Actor: "agent@host", Trigger: "branch"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	claimed, err := database.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinishRun(ctx, claimed.ID, model.RunResult{Status: model.RunPassed, Phase: "passed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	active, err := database.ActiveRunIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if name, err := database.RunWorkflowName(ctx, ids[2]); err != nil || name != "" {
		t.Fatalf("workflow of a queued run = %q, %v", name, err)
	}
	if _, err := database.RunWorkflowName(ctx, "no-such-run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	want := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == claimed.ID })
	if !slices.Equal(active, want) {
		t.Fatalf("active = %v, want the claimed and the queued run %v", active, want)
	}
}
