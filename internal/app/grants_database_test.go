package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestRevokedDatabaseGrantDeniedAtNextAdmission(t *testing.T) {
	f := newRaceFixture(t)
	grantedAdmissionSource(t, f.sourceDir)
	db, err := storetest.Open(t.Context(), filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	u, err := storetest.CreateUpstream(db, t.Context(), model.UpstreamSpec{Name: "upstream", Kind: "ssh", BaseURL: "ssh://source.invalid/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "test-repo", UpstreamID: u.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	const key = "upstream/acme/test-repo"
	if _, err := db.Grant(t.Context(), key, argoworkflow.BuildFile, "main", admissionSecretPath, "admin"); err != nil {
		t.Fatal(err)
	}
	f.jobs.secretAccess = db
	raw, err := os.ReadFile(filepath.Join(f.sourceDir, argoworkflow.BuildFile))
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := argoworkflow.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.jobs.stepSecretAccess(t.Context(), repo, workflow, argoworkflow.BuildFile); err != nil {
		t.Fatal("granted admission refused", err)
	}
	if _, err := db.Revoke(t.Context(), key, argoworkflow.BuildFile, "main", admissionSecretPath, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.jobs.stepSecretAccess(t.Context(), repo, workflow, argoworkflow.BuildFile); err == nil {
		t.Fatal("revoked grant survived the next admission")
	}
}
