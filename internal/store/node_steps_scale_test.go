package store

import (
	"context"
	"fmt"
	"strings"
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
	// The old three audit walks per node take over a minute on this fixture.
	// Allow ample shared-host headroom while bounding the entire snapshot.
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	started := time.Now()
	if err = s.PutStepResults(ctx, run.ID, rows); err != nil {
		t.Fatal("4096-row snapshot exceeded bounded write budget:", err)
	}
	t.Logf("4096 nodes / 50000 historical runs: %s (%.0f nodes/s)", time.Since(started), float64(len(rows))/time.Since(started).Seconds())
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
