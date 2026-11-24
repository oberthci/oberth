package store

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

type migrationFixture struct {
	SourceCommit string `json:"source_commit"`
	Schema       int    `json:"schema"`
	SHA256       string `json:"sha256"`
	Run          stateport.Run
	Repository   stateport.Repository
	Issues       []model.Issue
	Grants       stateport.AuthoritySnapshot
	Audit        []model.AuditAction
}

func readMigrationFixture(directory string, number int) ([]byte, migrationFixture, error) {
	var fixture migrationFixture
	name := filepath.Join(directory, fmt.Sprintf("schema%d", number))
	metadata, err := os.ReadFile(name + ".json")
	if err != nil {
		return nil, fixture, err
	}
	if err := json.Unmarshal(metadata, &fixture); err != nil {
		return nil, fixture, err
	}
	file, err := os.Open(name + ".sqlite.gz")
	if err != nil {
		return nil, fixture, err
	}
	defer func() { _ = file.Close() }()
	zipped, err := gzip.NewReader(file)
	if err != nil {
		return nil, fixture, err
	}
	defer func() { _ = zipped.Close() }()
	body, err := io.ReadAll(io.LimitReader(zipped, 32<<20))
	if err != nil {
		return nil, fixture, err
	}
	if err := fixture.validate(number, body); err != nil {
		return nil, fixture, err
	}
	return body, fixture, nil
}

func (f migrationFixture) validate(number int, body []byte) error {
	if f.Schema != number || len(f.SourceCommit) != 40 || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) {
		return fmt.Errorf("schema %d fixture provenance differs", number)
	}
	if len(f.Issues) < 2 || f.Run.ID == "" || f.Repository.ID == 0 || len(f.Grants.Grants) == 0 || len(f.Audit) == 0 {
		return fmt.Errorf("schema %d fixture has no preservation oracle", number)
	}
	return nil
}

func migrationFixturePath(t *testing.T, number int) (string, migrationFixture) {
	t.Helper()
	body, fixture, err := readMigrationFixture("testdata/migrations", number)
	requireOK(t, err)
	path := filepath.Join(t.TempDir(), "state.sqlite")
	requireOK(t, os.WriteFile(path, body, 0600))
	return path, fixture
}

func assertMigrationRecords(t *testing.T, s *Store, want migrationFixture) {
	t.Helper()
	run, err := s.Run(t.Context(), want.Run.ID)
	requireOK(t, err)
	repository, err := s.Repository(t.Context(), want.Repository.Canonical, nil)
	requireOK(t, err)
	issues, err := s.Issues(t.Context(), nil, model.IssueFilter{Limit: 100})
	requireOK(t, err)
	grants, err := s.Grants(t.Context(), nil)
	requireOK(t, err)
	audit, _, err := s.Audit(t.Context(), 0, 100)
	requireOK(t, err)
	got := want
	got.Run, got.Repository, got.Issues, got.Grants, got.Audit = run, repository, issues, grants, audit
	if !reflect.DeepEqual(got, want) {
		t.Fatal("migration changed issues, admission, repository, grants or audit", encode(got), encode(want))
	}
	_, err = s.Run(t.Context(), "missing-migration-run")
	requireCode(t, err, publicerr.NotFound)
	_, err = s.Repository(t.Context(), "origin/team/missing", nil)
	requireCode(t, err, publicerr.NotFound)
	_, err = s.ScopedIssue(t.Context(), stateport.IssueScope{}, 123456)
	requireCode(t, err, publicerr.NotFound)
}
