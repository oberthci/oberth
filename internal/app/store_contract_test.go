package app_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	statecontract "github.com/oberthci/oberth/internal/stateport/contracttest"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestLandedStoreSharedRunContract(t *testing.T) {
	t.Parallel()
	statecontract.RunSuite(t, landedStoreFixture)
}
func TestLandedStoreFakeSharedRunContract(t *testing.T) {
	t.Parallel()
	statecontract.RunSuite(t, landedFakeFixture)
}
func landedClock() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
func landedStoreFixture(t *testing.T) statecontract.RunFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := store.Create(t.Context(), dbPath, store.Options{Protocol: stateport.DefaultProtocol(), BusyTimeout: time.Second, Clock: landedClock})
	landedOK(t, err)
	t.Cleanup(func() { landedOK(t, s.Close()) })
	seed := func(t *testing.T, name string) {
		t.Helper()
		parts := strings.Split(name, "/")
		action := stateport.Action{Actor: "fixture"}
		landedOK(t, s.RegisterUpstream(t.Context(), action, stateport.Upstream{Name: parts[0], Org: parts[1], Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
		_, err := s.RegisterRepository(t.Context(), action, name)
		landedOK(t, err)
	}
	seed(t, "origin/team/repo")
	return statecontract.RunFixture{Port: s, Admit: landedAdmission, Repository: seed,
		Nodes: func(t *testing.T, id string, nodes []stateport.Node) { seedRawNodes(t, dbPath, id, nodes) },
		Archive: func(t *testing.T, name string) {
			t.Helper()
			r, err := s.Repository(t.Context(), name, nil)
			landedOK(t, err)
			landedOK(t, s.ArchiveRepository(t.Context(), stateport.Action{Actor: "fixture"}, r.ID))
		},
		Prune: func(t *testing.T) {
			t.Helper()
			cutoff := landedClock().AddDate(0, 0, 1)
			landedOK(t, s.RetainEvents(t.Context(), cutoff, cutoff))
		},
		NativeAdvance: func(t *testing.T, id string) {
			t.Helper()
			_, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: 1})
			landedOK(t, err)
		},
		Audit: func(t *testing.T) []model.AuditAction {
			t.Helper()
			rows, _, err := s.Audit(t.Context(), 0, 200)
			landedOK(t, err)
			return rows
		},
		Publication: s.Publication,
		Emit: func(ctx context.Context, events []stateport.EventWrite) error {
			return s.RecordAction(ctx, model.AuditAction{Actor: "fixture", Action: "fixture", ResourceType: "event", ResourceID: "events"}, events...)
		},
	}
}
func landedFakeFixture(t *testing.T) statecontract.RunFixture {
	t.Helper()
	s := statefake.NewRuns(landedClock)
	seed := func(t *testing.T, name string) {
		t.Helper()
		parts := strings.Split(name, "/")
		s.SeedRepository(stateport.Repository{Upstream: parts[0], Name: strings.Join(parts[1:], "/"), Canonical: name})
	}
	seed(t, "origin/team/repo")
	return statecontract.RunFixture{Port: s, Admit: landedAdmission, Repository: seed,
		Nodes: func(_ *testing.T, id string, nodes []stateport.Node) { s.SeedNodes(id, nodes) },
		Archive: func(t *testing.T, name string) {
			t.Helper()
			r, err := s.Repository(t.Context(), name, nil)
			landedOK(t, err)
			r.Archived = true
			s.SeedRepository(r)
		},
		Prune: func(t *testing.T) {
			t.Helper()
			landedOK(t, s.PruneEvents(t.Context()))
		},
		NativeAdvance: func(t *testing.T, id string) {
			t.Helper()
			landedOK(t, s.AdvanceNative(t.Context(), id))
		},
		Audit: func(t *testing.T) []model.AuditAction {
			t.Helper()
			return s.Audits()
		}, Publication: s.Publication,
		Emit: func(ctx context.Context, events []stateport.EventWrite) error {
			return s.RecordAction(ctx, model.AuditAction{Actor: "fixture", Action: "fixture", ResourceType: "event", ResourceID: "events"}, events...)
		},
	}
}

// Native admission gives the shared store suite an actual sealed one-step plan.
func landedAdmission(t *testing.T, id string, r model.CIRequest) *admissioncontract.PreparedAdmission {
	t.Helper()
	document := []byte(`{"apiVersion":"argoproj.io/v1alpha1","kind":"Workflow","spec":{"entrypoint":"root","templates":[{"name":"root","dag":{"tasks":[{"name":"build","template":"build"}]}},{"name":"build","container":{"image":"oberth.ci/build:1","command":["true"]}}]}}`)
	file, blob := ".oberth/build.yaml", strings.Repeat("c", 40)
	q := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Repository: r.Repo, RunID: id, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: *r.CommitSHA, Trigger: r.Trigger, ScheduleEntry: r.ScheduleEntry, SelectedFile: file, FileBlob: blob, Document: document,
		SourceReceipt:   sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: r.Repo, Commit: *r.CommitSHA}, ObjectSHA: r.ObjectSHA, File: file, Blob: blob, DocumentDigest: sourcecontract.Digest(document)},
		IdentityRequest: admissioncontract.IdentityRequest{Mode: admissioncontract.ModeSubmit, Namespace: "runs", PipelineAccount: "pipeline"},
	}
	hooks := admissioncontract.PrepareHooks{
		VerifyIdentities: func(ctx context.Context, q admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
			return admissioncontract.IdentityEvidence{Namespace: q.Namespace, Pipeline: admissioncontract.AccountEvidence{Name: q.PipelineAccount, UID: "pipeline-uid"}, State: admissioncontract.StateVerified}, ctx.Err()
		},
		TransformNative: func(ctx context.Context, w *wfv1.Workflow, _ admissioncontract.TransformInput) error {
			w.Name = id
			return ctx.Err()
		},
	}
	p, err := argoworkflow.Prepare(t.Context(), q, argoworkflow.Policy{}, hooks)
	landedOK(t, err)
	return p
}
func landedOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The shared reader case includes scaffolding outside the admitted plan, as its
// owner fixture does. These raw rows arrange that case; all run writes use Store.
func seedRawNodes(t *testing.T, path, id string, nodes []stateport.Node) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	landedOK(t, err)
	defer func() { landedOK(t, db.Close()) }()
	for _, node := range nodes {
		raw, err := json.Marshal(node)
		landedOK(t, err)
		_, err = db.ExecContext(t.Context(), `INSERT INTO nodes VALUES(?,?,?,?,?,?)`, id, node.NodeID, node.Path, false, string(raw), 1)
		landedOK(t, err)
	}
}
