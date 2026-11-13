package argojob

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/testdb"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// The fixture is the compiler output captured from e5421125, with a device
// map in its operator grant. Its Workflow and capability annotation are intact.
func TestOldDeviceGrantWorkflowRecoversThroughRealMigration(t *testing.T) {
	var fixture struct {
		Workflow *wfv1.Workflow
		Adoption AdoptionRequest
		Config   Config
		Grant    json.RawMessage
	}
	raw, err := os.ReadFile("testdata/adoption/device-grant.json")
	if err != nil || json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("read old compiler fixture", err)
	}
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "upgrade.sqlite")
	db, err := storetest.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := storetest.CreateUpstream(db, ctx, model.UpstreamSpec{Name: "forge", Kind: "forgejo", BaseURL: "https://forge.example"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil || repo.ID != fixture.Adoption.RepositoryID {
		t.Fatal("fixture repository identity", repo, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seed.Exec(testdb.PostExtendedResourceGrantDowngrade); err != nil {
		t.Fatal(err)
	}
	if _, err = seed.Exec(`INSERT INTO workflow_capability_grants(repo_id,capability_json,granted_by,granted_at) VALUES(?,?,'operator',123)`, repo.ID, string(fixture.Grant)); err != nil {
		t.Fatal(err)
	}
	if _, err = seed.Exec(`INSERT INTO runs(id,repo_id,ref_kind,ref,sha,tested_sha,actor,release,credentialed,trigger,phase,job_name,status,queued_at,started_at,created_at,updated_at,admission_sequence)
 VALUES(?,?,'branch',?,?,?,'author',0,0,'push','running',?,'running',100,101,100,101,1)`, fixture.Adoption.RunID, repo.ID, fixture.Adoption.Ref, fixture.Adoption.SHA, fixture.Adoption.SHA, fixture.Workflow.Name); err != nil {
		t.Fatal(err)
	}
	if err = seed.Close(); err != nil {
		t.Fatal(err)
	}
	// This production opener applies the pending SQL; no fabricated hash loader.
	db, err = store.OpenAdminClient(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	current, err := db.ActiveWorkflowCapability(ctx, repo.ID)
	if err != nil || current == nil {
		t.Fatal(current, err)
	}
	c := (&Controller{config: fixture.Config, workflows: &staticWorkflowClient{workflow: fixture.Workflow}, kube: operatorKubeClient(current)}).WithWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
	if _, _, err = c.Adopt(ctx, fixture.Adoption); !errors.Is(err, ErrWorkflowCapabilityChanged) {
		t.Fatal("fixture did not exercise the changed grant hash", err)
	}
	c.WithWorkflowCapabilityRecoveryLoader(db.WorkflowCapabilityRecovery)
	before, _ := json.Marshal(fixture.Workflow)
	if _, found, err := c.Adopt(ctx, fixture.Adoption); err != nil || !found {
		t.Fatal("authorized old-base Workflow lost on upgrade", found, err)
	}
	after, _ := json.Marshal(fixture.Workflow)
	if !bytes.Equal(before, after) {
		t.Fatal("adoption rewrote the Workflow")
	}
	// Ongoing executor-token recovery uses the same receipt and current scope.
	if _, err = c.boundCapability(ctx, fixture.Workflow, repo.ID); err != nil {
		t.Fatal("recovered executor identity lost its binding", err)
	}
	for _, mutation := range []string{"run", "workflow", "sha", "repo"} {
		r, name, sha, id := fixture.Adoption.RunID, fixture.Workflow.Name, fixture.Adoption.SHA, repo.ID
		switch mutation {
		case "run":
			r += "-new"
		case "workflow":
			name += "-new"
		case "sha":
			sha = "different"
		case "repo":
			id++
		}
		receipt, err := db.WorkflowCapabilityRecovery(ctx, id, r, name, sha)
		if err != nil || len(receipt) != 0 {
			t.Fatal("receipt escaped its submission", mutation, string(receipt), err)
		}
	}
	// New compiler output binds the current grant, without the device map.
	request := requestWithCapability(t, operatorResourceDocument, current)
	request.RepositoryID = repo.ID
	posted, err := Build(testConfig(), request)
	if err != nil || posted.Annotations[capabilityAnnotation] != capabilityBinding(repo.ID, current) || posted.Annotations[capabilityAnnotation] == fixture.Workflow.Annotations[capabilityAnnotation] {
		t.Fatal("new submission retained the old grant", err)
	}
	changed := *current
	changed.ResourceKinds = append([]argoworkflow.ResourceKindCapability(nil), current.ResourceKinds...)
	changed.ResourceKinds[0].Actions = []string{"create", "delete"}
	if _, err = db.SetWorkflowCapabilityGrant(ctx, repo.ID, "operator", &changed); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Adopt(ctx, fixture.Adoption); !errors.Is(err, ErrWorkflowCapabilityChanged) {
		t.Fatal("genuinely changed grant accepted", err)
	}
	// Returning to the same grant cannot resurrect a consumed authority epoch.
	if _, err = db.SetWorkflowCapabilityGrant(ctx, repo.ID, "operator", current); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Adopt(ctx, fixture.Adoption); !errors.Is(err, ErrWorkflowCapabilityChanged) {
		t.Fatal("re-grant resurrected old recovery authority", err)
	}
}
