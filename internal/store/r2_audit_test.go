package store

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func auditRowsForTest(t *testing.T, s *Store, n int) {
	t.Helper()
	for range n {
		requireOK(t, s.RecordAction(t.Context(), model.AuditAction{Actor: "operator", Action: "fixture.audit", ResourceType: "fixture"}))
	}
}

func damageAuditForTest(t *testing.T, s *Store, statement string) {
	t.Helper()
	var update, remove string
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name='audit_immutable'`).Scan(&update))
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name='audit_retained'`).Scan(&remove))
	execute(t, s.writer, `DROP TRIGGER audit_immutable`)
	execute(t, s.writer, `DROP TRIGGER audit_retained`)
	execute(t, s.writer, statement)
	execute(t, s.writer, update)
	execute(t, s.writer, remove)
}

func TestAuditPageVerifiesRowsAndPrecedingLink(t *testing.T) {
	t.Parallel()
	for _, statement := range []string{
		`UPDATE audit SET details='changed' WHERE id=4`,
		`UPDATE audit SET previous='wrong' WHERE id=4`,
		`UPDATE audit SET hash='wrong' WHERE id=3`,
		`DELETE FROM audit WHERE id=4`,
	} {
		t.Run(statement, func(t *testing.T) {
			s := testStore(t)
			auditRowsForTest(t, s, 6)
			rows, head, err := s.Audit(t.Context(), 3, 1)
			requireOK(t, err)
			if len(rows) != 1 || rows[0].ID != 4 || head.ID != 6 {
				t.Fatal("wrong audit page", rows, head)
			}
			damageAuditForTest(t, s, statement)
			_, _, err = s.Audit(t.Context(), 3, 1)
			requireCode(t, err, publicerr.SchemaRefused)
		})
	}
}

func TestAuditPageLeavesWholeChainVerificationToBackupAndVerify(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	auditRowsForTest(t, s, 6)
	damageAuditForTest(t, s, `UPDATE audit SET details='outside page' WHERE id=1`)
	rows, head, err := s.Audit(t.Context(), 4, 1)
	requireOK(t, err)
	if len(rows) != 1 || rows[0].ID != 5 || head.ID != 6 {
		t.Fatal("wrong bounded page", rows, head)
	}
	_, err = s.Snapshot(t.Context(), backupOptions(s), &bytes.Buffer{})
	requireCode(t, err, publicerr.SchemaRefused)
}

func TestArchiveRefusalNamesActiveWorkAndItsViews(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "active")
	err := s.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, r.RepositoryID)
	var reason, remedy string
	_, reason, remedy = message(repositoryBusy)
	requireCode(t, err, publicerr.Conflict)
	if !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), remedy) {
		t.Fatal("archive named a removal cause", err)
	}
}
