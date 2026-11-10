package store_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func restoredDatabase(t *testing.T, queued bool) string {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := storetest.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if queued {
		upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
		if err != nil {
			t.Fatal(err)
		}
		repo, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storetest.EnqueueRun(database, ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: "v1.0.0", Release: true,
			SHA: "dddddddddddddddddddddddddddddddddddddddd", Actor: "agent@host", Trigger: "tag"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A rollback to a server older than the restored-runs marker must not
// dispatch the backup's queued work: the restore places a hold (no class
// allowed, so not even the release tag runs) through the audited path, and a
// server opening the file afterwards sees it and an intact audit chain.
func TestPlaceRestoreHoldHoldsQueuedWork(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := restoredDatabase(t, true)
	placed, hold, previous, err := store.PlaceRestoreHold(ctx, path, store.Options{}, "oberth upgrade --rollback", "restored by oberth upgrade --rollback", 12*time.Hour)
	if err != nil || !placed || !hold.On || len(hold.Allow) != 0 || previous.On {
		t.Fatalf("placed=%v hold=%+v previous=%+v err=%v", placed, hold, previous, err)
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("WAL not folded into the main file: %d bytes", info.Size())
	}
	database, err := storetest.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("server open after the restore hold: %v", err)
	}
	defer func() { _ = database.Close() }()
	current, err := database.AdmissionHold(ctx)
	if err != nil || !current.On || current.Reason != "restored by oberth upgrade --rollback" {
		t.Fatalf("hold = %+v, %v", current, err)
	}
	if _, err := database.VerifyAuditChain(ctx); err != nil {
		t.Fatalf("audit chain after the restore hold: %v", err)
	}
	if run, err := database.ClaimNextRun(ctx); err == nil {
		t.Fatalf("held database dispatched %s", run.ID)
	}
}

// A hold the backup already has may allow release runs or lapse soon after
// the restore: it is replaced by the stricter, longer one, and a run that
// was running counts as work to hold (an older server requeues it when its
// Workflow is gone).
func TestPlaceRestoreHoldReplacesAWeakerHoldAndCountsRunningRuns(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := restoredDatabase(t, true)
	database, err := storetest.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimNextRun(ctx); err != nil { // the only run is now running
		t.Fatal(err)
	}
	if _, err := database.SetAdmissionHold(ctx, "operator", model.AdmissionHoldRequest{On: true, Reason: "maintenance", Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	placed, hold, previous, err := store.PlaceRestoreHold(ctx, path, store.Options{}, "oberth upgrade --rollback", "restored", 12*time.Hour)
	if err != nil || !placed || len(hold.Allow) != 0 || previous.Reason != "maintenance" || len(previous.Allow) != 2 {
		t.Fatalf("placed=%v hold=%+v previous=%+v err=%v", placed, hold, previous, err)
	}
	database, err = storetest.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	current, err := database.AdmissionHold(ctx)
	if err != nil || current.Reason != "restored" || len(current.Allow) != 0 || time.Until(current.ExpiresAt) < 11*time.Hour {
		t.Fatalf("hold = %+v, %v", current, err)
	}
	if _, err := database.VerifyAuditChain(ctx); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
}

func TestPlaceRestoreHoldSkipsWithoutQueuedWork(t *testing.T) {
	t.Parallel()
	path := restoredDatabase(t, false)
	placed, hold, _, err := store.PlaceRestoreHold(t.Context(), path, store.Options{}, "oberth upgrade --rollback", "reason", time.Hour)
	if err != nil || placed || hold.On {
		t.Fatalf("placed=%v hold=%+v err=%v", placed, hold, err)
	}
}
