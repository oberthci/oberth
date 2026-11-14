package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestNodeStepSnapshot4096RowsWith50000HistoricalRuns(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "scale", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunWorkflowName(t.Context(), run.ID, "workflow"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// Real historical runs, each with a valid chained submit receipt. Seed in
	// one transaction so fixture setup does not benchmark per-run commits.
	_, err = tx.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<50000)
 INSERT INTO runs(id,repo_id,ref_kind,ref,sha,actor,trigger,status,job_name,queued_at,created_at,updated_at)
 SELECT 'history-'||i,?,'branch','history',?,'actor','push','passed','workflow',1,1,1 FROM n`, repo.ID, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	const body = `{"engine":"argo","workflow":"workflow","step_credentials":[{"template":"leaf","known":true,"complete":true,"credentialed":false,"grants":[]}]}`
	for i := 1; i <= 50000; i++ {
		_, err = s.appendAuditAction(t.Context(), tx, model.AuditActionSpec{Actor: "actor", Action: "ci.argo.submit.binding", ResourceType: "run", ResourceID: fmt.Sprintf("history-%d", i), Details: body}, unixNano(now))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "actor", Action: "ci.argo.submit.binding", ResourceType: "run", ResourceID: run.ID, Details: body}); err != nil {
		t.Fatal(err)
	}
	rows := make([]model.StepResult, 4096)
	for i := range rows {
		rows[i] = model.StepResult{NodeID: fmt.Sprintf("node-%04d", i), Template: "leaf", LocalTemplate: true, Ordinal: i, Status: model.StepRunning}
	}
	// Assert work instead of elapsed time: the shared host may be busy.
	var seq int
	var dbName, dbPath string
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&seq, &dbName, &dbPath); err != nil {
		t.Fatal(err)
	}
	dsn, err := immediateTransactionDSN(dbPath, defaultBusyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	queries := &atomic.Int64{}
	counted := sql.OpenDB(nodeCountConnector{driver: s.db.Driver(), dsn: dsn, queries: queries})
	counted.SetMaxOpenConns(1)
	original := s.db
	s.db = counted
	defer func() { s.db = original; _ = counted.Close() }()
	if err = s.PutStepResults(t.Context(), run.ID, rows); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 1 {
		t.Fatalf("snapshot walked admission receipts %d times; want exactly one", got)
	}
	plan, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN SELECT 1"+stepAdmissionSourceSQL, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	indexed := false
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		indexed = indexed || strings.Contains(detail, "audit_actions_run_resource_action")
	}
	if err := plan.Close(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("admission read does not use the run-scoped receipt index")
	}
	stored, err := s.StepResults(t.Context(), run.ID)
	if err != nil || len(stored) != len(rows) {
		t.Fatal(len(stored), err)
	}
	for _, row := range stored {
		if !row.CredentialEvidenceKnown || row.Credentialed || !row.ReplayEvidenceComplete {
			t.Fatal("bound evidence lost", row.NodeID)
		}
	}
}

func TestNodeAdmissionRequiresStringTemplateIdentity(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "template-identity", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunWorkflowName(t.Context(), run.ID, "workflow"); err != nil {
		t.Fatal(err)
	}
	_, err = s.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "actor", Action: "ci.argo.submit.binding", ResourceType: "run", ResourceID: run.ID, Details: `{"engine":"argo","workflow":"workflow","step_credentials":[{"template":123,"known":true,"complete":true,"credentialed":false,"grants":[]}]}`})
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.PutStepResult(t.Context(), model.StepResult{RunID: run.ID, NodeID: "node", Template: "123", LocalTemplate: true, Status: model.StepPassed})
	if err != nil || row.CredentialEvidenceKnown {
		t.Fatal("non-string receipt gained native template authority", row, err)
	}
}

// Count every SQL read that references audit_actions, including a receipt walk
// embedded in a node upsert. No production hook or wall-clock bound is needed.
type nodeCountConnector struct {
	driver  driver.Driver
	dsn     string
	queries *atomic.Int64
}

func (c nodeCountConnector) Driver() driver.Driver { return c.driver }
func (c nodeCountConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &nodeCountConn{Conn: conn, queries: c.queries}, nil
}

type nodeCountConn struct {
	driver.Conn
	queries *atomic.Int64
}

func (c *nodeCountConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *nodeCountConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "audit_actions") {
		c.queries.Add(1)
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *nodeCountConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "audit_actions") {
		c.queries.Add(1)
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
