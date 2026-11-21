package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// PutNodes records one atomic observation batch of at most 128 rows.
// Callers may submit any number of batches; there is no per-run node ceiling.
func (s *Store) PutNodes(ctx context.Context, action stateport.Action, batch stateport.NodeBatch) error {
	runID, nodes := batch.RunID, batch.Nodes
	if len(nodes) < 1 || len(nodes) > 128 || batch.Offset < 0 {
		return refuse(invalid, nil)
	}
	return s.observe(ctx, func(tx *sql.Tx) error {
		if err := advanceEvidence(ctx, tx, batch); err != nil {
			return err
		}
		plan, err := readPlan(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := validateNodes(plan, nodes); err != nil {
			return err
		}
		for _, n := range nodes {
			if err := putNode(ctx, tx, nodeWrite{runID, n, batch.Sequence}); err != nil {
				return err
			}
		}
		return nil
	}, action.Events...)
}

type nodeWrite struct {
	runID    string
	node     stateport.Node
	sequence int64
}

func putNode(ctx context.Context, tx *sql.Tx, input nodeWrite) error {
	n := input.node
	var oldBody string
	var credential bool
	var sequence int64
	err := tx.QueryRowContext(ctx, `SELECT value,credential,sequence FROM nodes WHERE run_id=? AND node_id=?`, input.runID,
		n.NodeID).Scan(&oldBody, &credential, &sequence)
	if err == nil {
		if sequence == input.sequence {
			return refuse(conflict, nil)
		}
		old, err := decodeNode(oldBody)
		if err != nil {
			return refuse(corrupt, err)
		}
		if err := requireNodeUpdate(old, n, credential); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return databaseError(err)
	}
	credential = credential || n.Credential != nil && *n.Credential
	_, err = tx.ExecContext(ctx, `INSERT INTO nodes VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,node_id) DO UPDATE SET credential=excluded.credential,

value=excluded.value,sequence=excluded.sequence`, input.runID, n.NodeID, n.Path, credential, encode(n), input.sequence)
	return databaseError(err)
}

func nodeTerminal(status stepmodel.StepStatus) bool {
	return status == stepmodel.StepPassed || status == stepmodel.StepFailed || status == stepmodel.StepSkipped
}

func readNodes(ctx context.Context, tx *sql.Tx, runID string) ([]stateport.Node, error) {
	rows, err := tx.QueryContext(ctx, `SELECT value FROM nodes WHERE run_id=? ORDER BY node_id`, runID)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]stateport.Node, 0)
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, databaseError(err)
		}
		n, err := decodeNode(body)
		if err != nil {
			return nil, refuse(corrupt, err)
		}
		result = append(result, n)
	}
	return result, databaseError(rows.Err())
}

// CompleteEvidence refuses missing exits, credential observations or live attempts.
func (s *Store) CompleteEvidence(ctx context.Context, action stateport.Action, input stateport.EvidenceCompletion) error {
	runID := input.RunID
	return s.observe(ctx, func(tx *sql.Tx) error {
		if err := requireEvidenceSequence(ctx, tx, input); err != nil {
			return err
		}
		if err := requireTerminalEvidence(ctx, tx, runID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE runs SET evidence_complete=1 WHERE id=?`, runID)
		return databaseError(err)
	}, action.Events...)
}

func requirePlanPath(plan admissioncontract.Plan, n stateport.Node) error {
	if !stateport.NativePlanPath(plan, n.Path) {
		return refuse(invalid, nil)
	}
	return nil
}

func requireNodeUpdate(old, n stateport.Node, credential bool) error {
	if !sameNodeIdentity(old, n) {
		return refuse(nodeBinding, nil)
	}
	if old.PodName != "" && old.PodName != n.PodName {
		return refuse(nodeBinding, nil)
	}
	if credential && (n.Credential == nil || !*n.Credential) {
		return refuse(nodeBinding, nil)
	}
	if nodeTerminal(old.Status) && old.Status != n.Status {
		return refuse(nodeRegression, nil)
	}
	if !retainsNodeEvidence(old, n) {
		return refuse(nodeRetention, nil)
	}
	return nil
}

func retainsNodeEvidence(old, n stateport.Node) bool {
	return (!old.PodRestarted || n.PodRestarted) && retains(old.Exit, n.Exit) && retains(old.StartedAt, n.StartedAt) && retains(old.FinishedAt, n.FinishedAt)
}

func retains[T comparable](previous, next *T) bool {
	return previous == nil || next != nil && *previous == *next
}

func sameNodeIdentity(old, n stateport.Node) bool {
	return (len(old.Order) == 0 || slices.Equal(old.Order, n.Order)) && old.Path == n.Path && old.Attempt == n.Attempt && old.Template == n.Template && old.Boundary == n.Boundary && old.RetryOf == n.RetryOf
}

func validateNodes(plan admissioncontract.Plan, nodes []stateport.Node) error {
	for _, n := range nodes {
		if err := requirePlanPath(plan, n); err != nil {
			return err
		}
		if n.NodeID == "" || n.Attempt < 1 || !n.Status.Valid() {
			return refuse(invalid, nil)
		}
	}
	return nil
}

func requireTerminalEvidence(ctx context.Context, tx *sql.Tx, runID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT value FROM nodes WHERE run_id=?`, runID)
	if err != nil {
		return databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		found = true
		var body string
		if err := rows.Scan(&body); err != nil {
			return databaseError(err)
		}
		n, err := decodeNode(body)
		if err != nil {
			return refuse(corrupt, err)
		}
		if !nodeTerminal(n.Status) || n.Credential == nil || (n.Status == stepmodel.StepPassed && n.Exit == nil) {
			return refuse(incompleteEvidence, nil)
		}
	}
	if !found {
		return refuse(incompleteEvidence, nil)
	}
	return databaseError(rows.Err())
}
