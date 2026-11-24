package store

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// The closed fixture was created by main 9cb00e279e4475f52980a18446165d63c821932a,
// using Create, AcceptBatch, CreateIssue and ChangeGrant, before this change.
func TestSchema6DatabaseRetainsHistory(t *testing.T) {
	compressed, err := os.Open("testdata/schema6-history.sqlite.gz")
	requireOK(t, err)
	defer func() { requireOK(t, compressed.Close()) }()
	reader, err := gzip.NewReader(compressed)
	requireOK(t, err)
	defer func() { requireOK(t, reader.Close()) }()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	file, err := os.Create(path)
	requireOK(t, err)
	_, err = io.Copy(file, reader)
	requireOK(t, err)
	requireOK(t, file.Close())
	s, err := Open(t.Context(), path, testOptions())
	requireOK(t, err)
	defer func() { requireOK(t, s.Close()) }()
	before := independentHead(t, s.readers)
	checkSchema6History(t, s)
	// Opening a pre-change receipt must preserve its replay identity and must not
	// invent receive evidence from a run that had no recorded Git update.
	var body []byte
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT binding FROM receives WHERE id='receive-schema6-run'`).Scan(&body))
	var binding receiveBinding
	requireOK(t, json.Unmarshal(body, &binding))
	q := binding.Runs[0].Request
	run, err := s.Run(t.Context(), "schema6-run")
	requireOK(t, err)
	prepared, err := admissioncontract.Seal(run.Admission)
	requireOK(t, err)
	replay, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: q.ReceiveID, Repository: q.Repo, Actor: q.Actor, Runs: []stateport.Acceptance{{Request: q, Prepared: prepared}}})
	requireOK(t, err)
	if replay.Fresh || len(replay.Runs) != 1 {
		t.Fatal("schema 6 receipt did not replay", replay)
	}
	heads, err := s.ReceivedHeads(t.Context(), stateport.ReceivedRef{Repo: q.Repo, Ref: q.Ref, ObjectSHA: q.ObjectSHA, RunID: "schema6-run"})
	requireOK(t, err)
	if heads != nil {
		t.Fatal("old run acquired unrecorded receive evidence", heads)
	}
	if independentHead(t, s.readers) != before {
		t.Fatal("open or receipt replay changed durable state")
	}
	var tables int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE name='receive_heads'`).Scan(&tables))
	if Schema != 6 || tables != 0 {
		t.Fatal("receive evidence changed the durable schema", Schema, tables)
	}
	// New evidence is accepted into the same database, preserving every old row.
	q.ReceiveID, q.PreviousSHA = "schema6-new-receive", q.ObjectSHA
	q.ObjectSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	q.CommitSHA = new(q.ObjectSHA)
	_, err = s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: q.ReceiveID, Repository: q.Repo, Actor: q.Actor, Received: []model.CIRequest{q}})
	requireOK(t, err)
	checkSchema6History(t, s)
}

func checkSchema6History(t *testing.T, s *Store) {
	t.Helper()
	issue, err := s.Issue(t.Context(), 1)
	requireOK(t, err)
	if issue.Title != "retained issue" || issue.Body != "retained body" {
		t.Fatal("schema 6 issue changed", issue)
	}
	run, err := s.Run(t.Context(), "schema6-run")
	requireOK(t, err)
	if !run.HasAdmission || run.Repo != "origin/team/repo" || run.Admission.Plan.Entries[0].Path != "build" {
		t.Fatal("schema 6 run changed", run)
	}
	repo, err := s.Repository(t.Context(), run.Repo, nil)
	requireOK(t, err)
	if repo.ID != run.RepositoryID {
		t.Fatal("schema 6 repository identity changed", repo)
	}
	snapshot, err := s.Grants(t.Context(), nil)
	requireOK(t, err)
	if len(snapshot.Grants) != 1 || snapshot.Grants[0].Kind != stateport.GrantPVC || snapshot.Grants[0].Path != "cache" {
		t.Fatal("schema 6 grant changed", snapshot)
	}
}
