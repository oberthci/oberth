package store

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestRepositoryRemovalCheckUsesEveryDurableRemovalFence(t *testing.T) {
	for _, tc := range []struct{ name, statement string }{
		{"issue", `INSERT INTO issues(revision,upstream,repo,state,value) SELECT 1,upstream,canonical,'open','{}' FROM repositories WHERE id=?`},
		{"run", `INSERT INTO runs(id,repo_id,ref,status,release,origin,summary,step_count,steps_planned_exact,steps_pending) VALUES('run',?,'main','passed',0,'{}','{}',0,1,0)`},
		{"receive", `INSERT INTO receives(id,repo_id,binding) VALUES('receive',?,'{}')`},
		{"publication", `INSERT INTO publications(id,repo_id,status,value) VALUES('publication',?,'delivered','{}')`},
		{"promotion", `INSERT INTO promotions(id,repo_id,revision,status,caller,value) VALUES('promotion',?,1,'complete','operator','{}')`},
		{"claim", `INSERT INTO pvc_owners(claim,repo_id,tier) VALUES('claim',?,'ci')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			repo := atomicRepository(t, s)
			_, err := s.writer.ExecContext(t.Context(), tc.statement, repo.ID)
			requireOK(t, err)
			head := independentHead(t, s.readers)
			for _, err := range []error{
				s.CheckRepositoryRemoval(t.Context(), repo.ID),
				s.RemoveRepository(t.Context(), stateport.Action{Actor: "operator"}, repo.ID),
			} {
				requireCode(t, err, publicerr.Conflict)
				if publicerr.Classify(err).Reason() != "repository still owns durable history or storage" {
					t.Fatal("removal check disagrees with removal", err)
				}
			}
			got, err := s.Repository(t.Context(), repo.Canonical, nil)
			requireOK(t, err)
			if got != repo || independentHead(t, s.readers) != head {
				t.Fatal("refused removal changed repository or audit")
			}
		})
	}
}

func TestRepositoryRemovalCheckRollsBackCascadesWithoutAuditOrNotification(t *testing.T) {
	s := testStore(t)
	repo := atomicRepository(t, s)
	a := stateport.Action{Actor: "operator"}
	before, err := s.ChangeGrant(t.Context(), a, stateport.GrantChange{
		Kind: stateport.GrantStorePath, Repository: repo.Canonical, File: "build.yaml", Step: "build", Path: "registry/key",
	})
	requireOK(t, err)
	head, changes := independentHead(t, s.readers), s.Changes()
	requireOK(t, s.CheckRepositoryRemoval(t.Context(), repo.ID))
	got, err := s.Repository(t.Context(), repo.Canonical, nil)
	requireOK(t, err)
	after, err := s.Grants(t.Context(), nil)
	requireOK(t, err)
	if got != repo || !reflect.DeepEqual(before, after) || independentHead(t, s.readers) != head {
		t.Fatal("removal check changed repository, cascaded authority or audit")
	}
	select {
	case <-changes:
		t.Fatal("removal check published a change")
	default:
	}
	requireOK(t, s.RemoveRepository(t.Context(), a, repo.ID))
	_, err = s.Repository(t.Context(), repo.Canonical, nil)
	requireCode(t, err, publicerr.NotFound)
	requireCode(t, s.CheckRepositoryRemoval(t.Context(), repo.ID), publicerr.NotFound)
	after, err = s.Grants(t.Context(), nil)
	requireOK(t, err)
	if len(after.Grants) != 0 {
		t.Fatal("removal retained authority")
	}
}
