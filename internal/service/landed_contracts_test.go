package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Both implementations run the SAME suite and consume the actual admitted seal.
func TestLandedStoreSharedRunContracts(t *testing.T) {
	t.Parallel()
	t.Run("real", func(t *testing.T) { contracttest.RunSuite(t, landedRunFixture) })
	t.Run("fake", func(t *testing.T) {
		contracttest.RunSuite(t, func(t *testing.T) contracttest.RunFixture {
			f := ownerFixture(t)
			f.Admit = landedAdmission
			return f
		})
	})
}
func landedRunFixture(t *testing.T) contracttest.RunFixture {
	t.Helper()
	f := setup(t)
	state, path := landedStoreWithPath(t, f)
	register := func(t *testing.T, name string) { t.Helper(); registerContractRepository(t, state, name) }
	register(t, "origin/team/repo")
	return contracttest.RunFixture{Port: state, Admit: landedAdmission, Repository: register,
		Nodes: func(t *testing.T, id string, nodes []stateport.Node) { landedRawNodes(t, path, id, nodes) },
		Archive: func(t *testing.T, name string) {
			t.Helper()
			r, err := state.Repository(t.Context(), name, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := state.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, r.ID); err != nil {
				t.Fatal(err)
			}
		},
		Prune: func(t *testing.T) {
			t.Helper()
			cutoff := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			if err := state.RetainEvents(t.Context(), cutoff, cutoff); err != nil {
				t.Fatal(err)
			}
		},
		NativeAdvance: func(t *testing.T, id string) {
			t.Helper()
			if _, err := state.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: 1}); err != nil {
				t.Fatal(err)
			}
		},
		Audit: func(t *testing.T) []model.AuditAction {
			t.Helper()
			rows, _, err := state.Audit(t.Context(), 0, 200)
			if err != nil {
				t.Fatal(err)
			}
			return rows
		},
		Publication: state.Publication,
		Emit: func(ctx context.Context, events []stateport.EventWrite) error {
			return state.RecordEvents(ctx, events)
		},
	}
}
func registerContractRepository(t *testing.T, state *store.Store, name string) {
	t.Helper()
	parts := strings.Split(name, "/")
	action := stateport.Action{Actor: "operator"}
	if err := state.RegisterUpstream(t.Context(), action, stateport.Upstream{Name: parts[0], Org: parts[1], Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.RegisterRepository(t.Context(), action, name); err != nil {
		t.Fatal(err)
	}
}
func landedAdmission(t *testing.T, id string, r model.CIRequest) *admissioncontract.PreparedAdmission {
	t.Helper()
	document := []byte(`{"apiVersion":"argoproj.io/v1alpha1","kind":"Workflow","spec":{"entrypoint":"main","templates":[{"name":"main","dag":{"tasks":[{"name":"build","template":"build"}]}},{"name":"build","container":{"image":"build","command":["true"]}}]}}`)
	file := ".oberth/build.yaml"
	blob := strings.Repeat("b", 40)
	request := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, RunID: id, Repository: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: *r.CommitSHA, Trigger: r.Trigger, ScheduleEntry: r.ScheduleEntry, SelectedFile: file, FileBlob: blob, Document: document,
		SourceReceipt:   sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: r.Repo, Commit: *r.CommitSHA}, ObjectSHA: r.ObjectSHA, File: file, Blob: blob, DocumentDigest: sourcecontract.Digest(document)},
		IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: "step"},
	}
	hooks := admissioncontract.PrepareHooks{VerifyIdentities: func(_ context.Context, q admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
		return admissioncontract.IdentityEvidence{Namespace: q.Namespace, Pipeline: admissioncontract.AccountEvidence{Name: q.PipelineAccount, UID: "step-uid"}, State: admissioncontract.StateVerified}, nil
	}}
	if r.Assist {
		hooks.TransformNative = func(_ context.Context, _ *wfv1.Workflow, in admissioncontract.TransformInput) error {
			for i := range in.Plan.Entries {
				in.Plan.Entries[i].HoldArmed = true
			}
			return nil
		}
	}
	prepared, err := argoworkflow.Prepare(t.Context(), request, argoworkflow.Policy{}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

// Raw observations include scaffolding so the read contract exercises its filtering.
func landedRawNodes(t *testing.T, path, id string, nodes []stateport.Node) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, node := range nodes {
		body, err := json.Marshal(node)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(t.Context(), `INSERT INTO nodes(run_id,node_id,path,credential,value,sequence) VALUES(?,?,?,0,?,1)`, id, node.NodeID, node.Path, string(body)); err != nil {
			t.Fatal(err)
		}
	}
}
