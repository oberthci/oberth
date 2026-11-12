package installer

import (
	"path/filepath"
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
