package argojob_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/oberthci/oberth/internal/nativebinding"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	statecontract "github.com/oberthci/oberth/internal/stateport/contracttest"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/store/storetest"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func compositionClock() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
func compositionOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func compositionStore(t *testing.T) *store.Store {
	t.Helper()
	s, _ := compositionStorePath(t)
	return s
}

//nolint:gochecknoglobals // Store handles and clocks stay private to each copied baseline.
var compositionBaseline = storetest.New(func(t *testing.T, path string) {
	s, err := store.Create(t.Context(), path, store.DefaultOptions(compositionClock))
	compositionOK(t, err)
	compositionOK(t, s.Close())
})

func compositionStorePath(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := compositionBaseline.Path(t)
	s, err := store.Open(t.Context(), path, store.DefaultOptions(compositionClock))
	compositionOK(t, err)
	t.Cleanup(func() { compositionOK(t, s.Close()) })
	return s, path
}
func registerCompositionRepository(t *testing.T, s *store.Store, name string) {
	t.Helper()
	parts := strings.Split(name, "/")
	compositionOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, store.Upstream{Name: parts[0], Org: parts[1], Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	_, err := s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, name)
	compositionOK(t, err)
}
func compositionAdmission(t *testing.T, id string, r model.CIRequest) *admissioncontract.PreparedAdmission {
	t.Helper()
	document := []byte(`{"apiVersion":"argoproj.io/v1alpha1","kind":"Workflow","spec":{"entrypoint":"main","templates":[{"name":"main","dag":{"tasks":[{"name":"build","template":"build"}]}},{"name":"build","container":{"image":"example.test/build:1","command":["true"]}}]}}`)
	return compositionDocument(t, admissionInput{id: id, request: r, document: document})
}

type admissionInput struct {
	id       string
	request  model.CIRequest
	document []byte
}

func compositionDocument(t *testing.T, in admissionInput) *admissioncontract.PreparedAdmission {
	t.Helper()
	id, r, document := in.id, in.request, in.document
	pipeline, err := identityname.Name(identityname.Key{Repository: r.Repo}, identityname.PipelineCI)
	compositionOK(t, err)
	file, blob := ".oberth/build.yaml", strings.Repeat("b", 40)
	request := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Repository: r.Repo, RunID: id, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: *r.CommitSHA, Trigger: r.Trigger, ScheduleEntry: r.ScheduleEntry, SelectedFile: file, FileBlob: blob, Document: document,
		SourceReceipt:   sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: r.Repo, Commit: *r.CommitSHA}, ObjectSHA: r.ObjectSHA, File: file, Blob: blob, DocumentDigest: sourcecontract.Digest(document)},
		IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: pipeline}}
	e := argojob.New(argojob.Config{Accounts: nativefake.Accounts{Names: map[string]string{pipeline: "pipeline-uid"}}})
	p, err := argoworkflow.Prepare(t.Context(), request, argoworkflow.Policy{}, e.Hooks(nativebinding.Preparation{Repository: r.Repo, File: file}))
	compositionOK(t, err)
	return p
}
func realRunFixture(t *testing.T) statecontract.RunFixture {
	s, path := compositionStorePath(t)
	registerCompositionRepository(t, s, "origin/team/repo")
	return statecontract.RunFixture{Port: s, Admit: compositionAdmission, Publication: s.Publication,
		Nodes:      func(t *testing.T, id string, nodes []stateport.Node) { compositionRawNodes(t, path, id, nodes) },
		Repository: func(t *testing.T, name string) { registerCompositionRepository(t, s, name) },
		Archive: func(t *testing.T, name string) {
			r, err := s.Repository(t.Context(), name, nil)
			compositionOK(t, err)
			compositionOK(t, s.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, r.ID))
		},
		NativeAdvance: func(t *testing.T, id string) {
			_, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: 1})
			compositionOK(t, err)
		},
		Prune: func(t *testing.T) {
			cutoff := compositionClock().Add(24 * time.Hour)
			compositionOK(t, s.RetainEvents(t.Context(), cutoff, cutoff))
		},
		Audit: func(t *testing.T) []model.AuditAction {
			rows, _, err := s.Audit(t.Context(), 0, 200)
			compositionOK(t, err)
			return rows
		},
		Emit: func(ctx context.Context, events []stateport.EventWrite) error {
			return s.RecordEvents(ctx, events)
		},
	}
}
func memoryRunFixture(t *testing.T) statecontract.RunFixture {
	s := statefake.NewRuns(compositionClock)
	seed := func(t *testing.T, name string) {
		t.Helper()
		parts := strings.Split(name, "/")
		s.SeedRepository(stateport.Repository{Upstream: parts[0], Name: strings.Join(parts[1:], "/"), Canonical: name})
	}
	seed(t, "origin/team/repo")
	return statecontract.RunFixture{Port: s, Admit: compositionAdmission, Publication: s.Publication, Repository: seed,
		Nodes: func(_ *testing.T, id string, nodes []stateport.Node) { s.SeedNodes(id, nodes) },
		Archive: func(t *testing.T, name string) {
			r, err := s.Repository(t.Context(), name, nil)
			compositionOK(t, err)
			r.Archived = true
			s.SeedRepository(r)
		},
		NativeAdvance: func(t *testing.T, id string) { compositionOK(t, s.AdvanceNative(t.Context(), id)) },
		Prune:         func(t *testing.T) { compositionOK(t, s.PruneEvents(t.Context())) },
		Audit:         func(*testing.T) []model.AuditAction { return s.Audits() },
		Emit: func(ctx context.Context, events []stateport.EventWrite) error {
			return s.RecordEvents(ctx, events)
		},
	}
}

// Raw fixture facts exercise the shared read contract, including hidden structural nodes.
func compositionRawNodes(t *testing.T, path, id string, nodes []stateport.Node) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	compositionOK(t, err)
	defer func() { compositionOK(t, db.Close()) }()
	for _, n := range nodes {
		body, err := json.Marshal(n)
		compositionOK(t, err)
		_, err = db.ExecContext(t.Context(), `INSERT INTO nodes(run_id,node_id,path,credential,value,sequence) VALUES(?,?,?,0,?,1)`, id, n.NodeID, n.Path, string(body))
		compositionOK(t, err)
	}
}
