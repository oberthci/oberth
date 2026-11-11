package installer

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func queuedRestoreArchive(t *testing.T) restoreArchive {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := storetest.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.EnqueueRun(database, ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main",
		SHA: "dddddddddddddddddddddddddddddddddddddddd", Actor: "agent@host", Trigger: "push"}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := restoreArchive{Database: restoredDatabase{Main: path, MainSHA256: sum}, LocalDatabase: path}
	archive.Manifest.Compatibility = defaultCompat
	return archive
}

// A rollback onto a release older than v0.21.0 (which does not read the
// restored-runs marker) holds admission in the restored database and says
// how to release it; a v0.21+ target and an ordinary restore do not.
func TestRollbackRestoreHoldsAdmissionForOlderReleases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cfg     RestoreConfig
		version string
		hold    bool
	}{
		{"rollback to v0.20.1", RestoreConfig{InterruptQueued: true}, "0.20.1", true},
		{"rollback to unknown version", RestoreConfig{InterruptQueued: true}, "", true},
		{"rollback to v0.21 build", RestoreConfig{InterruptQueued: true}, "0.21.0-e2e.a.1", false},
		{"plain restore", RestoreConfig{}, "0.20.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			archive := queuedRestoreArchive(t)
			before := archive.Database.MainSHA256
			var out bytes.Buffer
			got, err := placeRollbackHold(t.Context(), tc.cfg, archive, restoreTarget{ChartVersion: tc.version}, &out)
			if err != nil {
				t.Fatal(err)
			}
			if changed := got.Database.MainSHA256 != before; changed != tc.hold {
				t.Fatalf("database changed=%v, want %v; output %q", changed, tc.hold, out.String())
			}
			if sum, _ := fileSHA256(got.Database.Main); sum != got.Database.MainSHA256 {
				t.Fatal("the swap would check a stale hash")
			}
			if tc.hold != strings.Contains(out.String(), `oberth admission hold off --reason "restored runs checked"`) {
				t.Fatalf("release command printed=%v: %q", !tc.hold, out.String())
			}
			database, err := storetest.Open(t.Context(), got.Database.Main, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = database.Close() }()
			hold, err := database.AdmissionHold(t.Context())
			if err != nil || hold.On != tc.hold || (tc.hold && hold.Reason != rollbackHoldReason) {
				t.Fatalf("hold = %+v, %v", hold, err)
			}
			// restore.completed names the head the server starts on: the
			// hold's audit action when one was placed, else the manifest's.
			head, err := database.VerifyAuditChain(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if tc.hold != (got.restoredHeadID() == head.ID && got.restoredHeadSHA256() == hex.EncodeToString(head.SHA256)) {
				t.Fatalf("restored head %d:%s, database head %d:%x (hold %v)", got.restoredHeadID(), got.restoredHeadSHA256(), head.ID, head.SHA256, tc.hold)
			}
		})
	}
}
