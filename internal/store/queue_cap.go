package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

const notStartedSQL = `(status='queued' OR (status='running' AND started_at IS NULL))`

type queueCapReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// PriorityRequest hides transport authority from public JSON. Retain the exact
// key and supersession choice here so the drain rechecks current permissions;
// never persist an API actor's cached administrator boolean.
type deferredBranchSpec struct {
	Run                  model.RunSpec
	RequesterFingerprint string
	Supersede            bool
}

func queueCap(ctx context.Context, reader queueCapReader) (model.QueueCapStatus, error) {
	var value model.QueueCapStatus
	err := reader.QueryRowContext(ctx, `SELECT max_not_started,
 (SELECT COUNT(*) FROM runs WHERE `+notStartedSQL+`),
 (SELECT COUNT(*) FROM deferred_branch_receives) FROM queue_cap WHERE singleton=1`).Scan(&value.MaxNotStarted, &value.NotStarted, &value.Deferred)
	if err != nil {
		return value, fmt.Errorf("read queue cap: %w", err)
	}
	if value.Deferred > 0 {
		value.Reason = fmt.Sprintf("branch run deferred: queue at %d/%d", value.NotStarted, value.MaxNotStarted)
		if value.MaxNotStarted == 0 {
			value.Reason = "queue cap off; draining deferred branch runs"
		}
		if value.MaxNotStarted > 0 && value.NotStarted < value.MaxNotStarted {
			value.Reason = "queue capacity available; draining deferred branch runs"
		}
	}
	return value, nil
}

func (s *Store) QueueCap(ctx context.Context) (model.QueueCapStatus, error) {
	return queueCap(ctx, s.reader())
}

// SetQueueCap replaces the installation-wide admin setting and records the
// previous value in the same transaction. Zero releases the deferred backlog.
func (s *Store) SetQueueCap(ctx context.Context, actor string, maxNotStarted int) (model.QueueCapStatus, error) {
	if strings.TrimSpace(actor) == "" || maxNotStarted < 0 {
		return model.QueueCapStatus{}, fmt.Errorf("%w: actor and nonnegative maxNotStarted required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.QueueCapStatus{}, err
	}
	defer func() { _ = tx.Rollback() }()
	previous, err := queueCap(ctx, tx)
	if err != nil {
		return model.QueueCapStatus{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE queue_cap SET max_not_started=? WHERE singleton=1`, maxNotStarted); err != nil {
		return model.QueueCapStatus{}, err
	}
	details, err := json.Marshal(map[string]int{"maxNotStarted": maxNotStarted, "previousMaxNotStarted": previous.MaxNotStarted})
	if err != nil {
		return model.QueueCapStatus{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "queue.cap.config", ResourceType: "queue", ResourceID: "cap", Details: string(details)}, unixNano(s.now())); err != nil {
		return model.QueueCapStatus{}, err
	}
	value, err := queueCap(ctx, tx)
	if err != nil {
		return model.QueueCapStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.QueueCapStatus{}, err
	}
	return value, nil
}

func cappedBranch(spec model.RunSpec) bool {
	return spec.RefKind == model.RefBranch && !spec.Release && (spec.Trigger == "branch" || spec.Trigger == "push")
}

// deferBranchTx runs under the enqueue's immediate writer transaction: count,
// FIFO decision, receive acknowledgement and audit cannot race another arrival.
func (s *Store) deferBranchTx(ctx context.Context, tx *sql.Tx, event model.ReceiveEventSpec, spec model.RunSpec) (string, error) {
	if !cappedBranch(spec) {
		return "", nil
	}
	if err := requireRepositoryActive(ctx, tx, spec.RepoID); err != nil {
		return "", err
	}
	cap, err := queueCap(ctx, tx)
	if err != nil {
		return "", err
	}
	// Once a backlog exists, new branch arrivals join its tail even if a slot
	// just opened. Only the drain may claim that slot for the oldest receive.
	if cap.Deferred == 0 && (cap.MaxNotStarted == 0 || cap.NotStarted < cap.MaxNotStarted) {
		return "", nil
	}
	reason := fmt.Sprintf("branch run deferred: queue at %d/%d", cap.NotStarted, cap.MaxNotStarted)
	auditReason := reason
	if spec.Reason != "" {
		auditReason += "; " + spec.Reason
	}
	body, err := json.Marshal(deferredBranchSpec{Run: spec, RequesterFingerprint: spec.RequesterFingerprint, Supersede: spec.Supersede})
	if err != nil {
		return "", err
	}
	now := unixNano(s.now())
	if err := insertReceiveEvent(ctx, tx, event, "", now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deferred_branch_receives(event_id,run_spec,reason) VALUES(?,?,?)`, event.ID, string(body), reason); err != nil {
		return "", err
	}
	if err := s.appendReceiveAudit(ctx, tx, event, "", now, auditReason); err != nil {
		return "", err
	}
	return reason, nil
}

// DrainDeferredBranch creates at most one run, in durable arrival order, when
// a slot is available. The receive gains its run ID atomically; replay cannot
// create another run. Archived repositories wait until unarchived and do not
// hold up active repositories.
func (s *Store) DrainDeferredBranch(ctx context.Context) (model.EnqueueRunResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	cap, err := queueCap(ctx, tx)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if cap.Deferred == 0 || (cap.MaxNotStarted > 0 && cap.NotStarted >= cap.MaxNotStarted) {
		return model.EnqueueRunResult{}, nil
	}
	var id, body, reason string
	err = tx.QueryRowContext(ctx, `SELECT d.event_id,d.run_spec,d.reason FROM deferred_branch_receives d
 JOIN receive_events e ON e.id=d.event_id JOIN repositories r ON r.id=e.repo_id
 WHERE `+repositoryArchiveAt+` IS NULL ORDER BY d.sequence LIMIT 1`).Scan(&id, &body, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return model.EnqueueRunResult{}, nil
	}
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	var saved deferredBranchSpec
	if err := json.Unmarshal([]byte(body), &saved); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("decode deferred branch: %w", err)
	}
	spec := saved.Run
	spec.RequesterFingerprint = saved.RequesterFingerprint
	spec.Supersede = saved.Supersede
	spec, err = validateRunSpec(spec)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if !cappedBranch(spec) {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: deferred receive is not a branch push", ErrInvalidState)
	}
	event, err := scanReceiveEvent(tx.QueryRowContext(ctx, `SELECT `+receiveEventColumns+` FROM receive_events WHERE id=?`, id))
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := matchReceiveRun(model.ReceiveEventSpec{Actor: event.Actor, RepoID: event.RepoID, RefKind: event.RefKind, Ref: event.Ref, ObjectSHA: event.ObjectSHA, CommitSHA: event.CommitSHA}, spec); err != nil {
		return model.EnqueueRunResult{}, err
	}
	runID, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	now := unixNano(s.now())
	result, err := s.enqueueRunTx(ctx, tx, spec, runID, now, 0)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if spec.Reason != "" {
		reason += "; " + spec.Reason
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET reason=? WHERE id=?`, reason, runID); err != nil {
		return model.EnqueueRunResult{}, err
	}
	result.Reason = reason
	if _, err := tx.ExecContext(ctx, `UPDATE receive_events SET run_id=? WHERE id=? AND run_id=''`, runID, id); err != nil {
		return model.EnqueueRunResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM deferred_branch_receives WHERE event_id=?`, id); err != nil {
		return model.EnqueueRunResult{}, err
	}
	details, err := json.Marshal(map[string]string{"event_id": id, "run_id": runID})
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: "oberth", Action: "queue.cap.drain", ResourceType: "receive", ResourceID: id, Details: string(details)}, now); err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.EnqueueRunResult{}, err
	}
	return result, nil
}

// DeferredBranchReason is scoped by a repository already resolved by the API.
func (s *Store) DeferredBranchReason(ctx context.Context, repoID int64, ref, sha string) (string, error) {
	var reason string
	err := s.reader().QueryRowContext(ctx, `SELECT d.reason FROM deferred_branch_receives d
 JOIN receive_events e ON e.id=d.event_id WHERE e.repo_id=? AND e.ref=? AND e.commit_sha=?
 ORDER BY d.sequence DESC LIMIT 1`, repoID, ref, strings.ToLower(sha)).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return reason, err
}
