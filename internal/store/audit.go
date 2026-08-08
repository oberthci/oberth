package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

const maxAuditReceiptBytes = 4 << 20

var auditGenesisSHA256 = make([]byte, sha256.Size)

type auditExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type auditStateQuerier interface {
	publicationAuditQuerier
}

type auditMutationSnapshot struct {
	head                     model.AuditHead
	byID                     map[int64][sha256.Size]byte
	latestPublicationActions map[string]auditPublicationAction
}

type auditPublicationAction struct {
	actor   string
	action  string
	details string
}

type auditMutationMetrics struct {
	fullTraversals int
	readStatements int
}

type observedAuditStateQuerier struct {
	auditStateQuerier
	fullTraversals int
	readStatements int
}

func (querier *observedAuditStateQuerier) recordFullAuditTraversal() {
	querier.fullTraversals++
}

func (querier *observedAuditStateQuerier) metrics() auditMutationMetrics {
	return auditMutationMetrics{
		fullTraversals: querier.fullTraversals,
		readStatements: querier.readStatements,
	}
}

func (querier *observedAuditStateQuerier) QueryContext(
	ctx context.Context,
	query string,
	args ...any,
) (*sql.Rows, error) {
	querier.readStatements++
	return querier.auditStateQuerier.QueryContext(ctx, query, args...)
}

func (querier *observedAuditStateQuerier) QueryRowContext(
	ctx context.Context,
	query string,
	args ...any,
) *sql.Row {
	querier.readStatements++
	return querier.auditStateQuerier.QueryRowContext(ctx, query, args...)
}

func appendAuditAction(ctx context.Context, execer auditExecer, spec model.AuditActionSpec, createdAt int64) (model.AuditAction, error) {
	if strings.TrimSpace(spec.Actor) == "" || strings.TrimSpace(spec.Action) == "" || strings.TrimSpace(spec.ResourceType) == "" {
		return model.AuditAction{}, fmt.Errorf("%w: audit action fields are required", ErrInvalid)
	}
	if createdAt <= 0 {
		return model.AuditAction{}, fmt.Errorf("%w: audit action timestamp must be positive", ErrInvalid)
	}
	if spec.Details == "" {
		spec.Details = "{}"
	}
	if !json.Valid([]byte(spec.Details)) {
		return model.AuditAction{}, fmt.Errorf("%w: audit details must be JSON", ErrInvalid)
	}

	id := int64(1)
	previous := append([]byte(nil), auditGenesisSHA256...)
	var previousID int64
	var storedPrevious []byte
	err := execer.QueryRowContext(ctx, `SELECT id, sha256 FROM audit_actions ORDER BY id DESC LIMIT 1`).Scan(&previousID, &storedPrevious)
	if err == nil {
		if len(storedPrevious) != sha256.Size {
			return model.AuditAction{}, fmt.Errorf("audit chain head has %d-byte hash", len(storedPrevious))
		}
		id = previousID + 1
		previous = append(previous[:0], storedPrevious...)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return model.AuditAction{}, fmt.Errorf("read audit chain head: %w", err)
	}
	digest := auditActionSHA256(id, previous, spec, createdAt)
	if _, err := execer.ExecContext(ctx, `
INSERT INTO audit_actions(id, actor, action, resource_type, resource_id, details, previous_sha256, sha256, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, spec.Actor, spec.Action, spec.ResourceType, spec.ResourceID,
		spec.Details, previous, digest, createdAt); err != nil {
		return model.AuditAction{}, fmt.Errorf("insert chained audit action: %w", err)
	}
	return model.AuditAction{
		ID: id, Actor: spec.Actor, Action: spec.Action, ResourceType: spec.ResourceType,
		ResourceID: spec.ResourceID, Details: spec.Details, PreviousSHA256: previous,
		SHA256: digest, CreatedAt: fromUnixNano(createdAt),
	}, nil
}

func auditActionSHA256(id int64, previous []byte, spec model.AuditActionSpec, createdAt int64) []byte {
	digest := sha256.New()
	// Compatibility-frozen domain-separation tag: changing it breaks hash-chain
	// verification of every existing audit record. Never rename in place.
	_, _ = digest.Write([]byte("cloudtaser-oberth-audit-v1\x00"))
	writeAuditUint64(digest, uint64(id)) // #nosec G115 -- audit IDs are created as positive, monotonically increasing int64 values.
	writeAuditBytes(digest, previous)
	writeAuditString(digest, spec.Actor)
	writeAuditString(digest, spec.Action)
	writeAuditString(digest, spec.ResourceType)
	writeAuditString(digest, spec.ResourceID)
	writeAuditString(digest, spec.Details)
	writeAuditUint64(digest, uint64(createdAt)) // #nosec G115 -- appendAuditAction rejects non-positive timestamps.
	return digest.Sum(nil)
}

func writeAuditString(destination hash.Hash, value string) {
	writeAuditBytes(destination, []byte(value))
}

func writeAuditBytes(destination hash.Hash, value []byte) {
	writeAuditUint64(destination, uint64(len(value)))
	_, _ = destination.Write(value)
}

func writeAuditUint64(destination hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = destination.Write(encoded[:])
}

// VerifyAuditChain recomputes every link from the fixed genesis value. It
// rejects altered records, missing sequence numbers, and broken predecessor
// links before returning the current head.
func (s *Store) VerifyAuditChain(ctx context.Context) (model.AuditHead, error) {
	return verifyAuditChain(ctx, s.db)
}

// SoleAuditAction verifies the complete audit chain and returns its only
// action. It fails unless the verified chain contains exactly one action, so
// callers can rely on the result being the first entry of a virgin chain.
func (s *Store) SoleAuditAction(ctx context.Context) (model.AuditAction, error) {
	var sole model.AuditAction
	head, err := walkVerifiedAuditChain(ctx, s.db, func(action model.AuditAction) {
		if action.ID == 1 {
			sole = action
		}
	})
	if err != nil {
		return model.AuditAction{}, err
	}
	if head.ID != 1 {
		return model.AuditAction{}, fmt.Errorf("%w: audit chain has %d actions, want exactly one", ErrInvalidState, head.ID)
	}
	return sole, nil
}

// VerifyAuditState verifies the chain and every externally actionable pending
// row from one SQLite snapshot. Holding the transaction across both checks
// prevents a publication intent from changing between chain verification and
// intent comparison.
func (s *Store) VerifyAuditState(ctx context.Context) (model.AuditHead, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AuditHead{}, fmt.Errorf("begin audit state verification: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	head, err := verifyAuditChain(ctx, tx)
	if err != nil {
		return model.AuditHead{}, err
	}
	if err := verifyPendingPublicationIntents(ctx, tx); err != nil {
		return model.AuditHead{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.AuditHead{}, fmt.Errorf("finish audit state verification: %w", err)
	}
	return head, nil
}

// AuditHeadHint returns the latest stored audit head without treating it as
// verified. The mutation gate uses this value only to bound read-only witness
// recovery; VerifyAuditMutationState subsequently recomputes the complete
// chain in one authoritative SQLite snapshot.
func (s *Store) AuditHeadHint(ctx context.Context) (model.AuditHead, error) {
	var head model.AuditHead
	err := s.db.QueryRowContext(ctx, `SELECT id, sha256 FROM audit_actions ORDER BY id DESC LIMIT 1`).Scan(&head.ID, &head.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return model.AuditHead{SHA256: append([]byte(nil), auditGenesisSHA256...)}, nil
	}
	if err != nil {
		return model.AuditHead{}, fmt.Errorf("read audit head hint: %w", err)
	}
	if head.ID <= 0 || len(head.SHA256) != sha256.Size {
		return model.AuditHead{}, errors.New("audit head hint is invalid")
	}
	head.SHA256 = append([]byte(nil), head.SHA256...)
	return head, nil
}

// VerifyAuditMutationState makes the local half of the mutation-gate decision
// from one SQLite transaction and one complete audit_actions stream. It invokes
// whileVerified before committing so that external verification and continuity
// reconciliation run while SQLite excludes concurrent writers. The callback
// must not call Store methods because this Store owns a single database
// connection. No result is cached across gate invocations.
func (s *Store) VerifyAuditMutationState(
	ctx context.Context,
	intents []model.AuditWitnessIntent,
	witnesses []model.AuditWitness,
	whileVerified func(model.AuditHead, model.AuditAnchor) error,
) (model.AuditHead, model.AuditAnchor, error) {
	if whileVerified == nil {
		return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("%w: audit mutation verification callback is required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("begin audit mutation verification: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	head, anchor, _, err := verifyAuditMutationState(ctx, tx, intents, witnesses)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, err
	}
	if err := whileVerified(head, anchor); err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, fmt.Errorf("finish audit mutation verification: %w", err)
	}
	return head, anchor, nil
}

func verifyAuditMutationState(
	ctx context.Context,
	querier auditStateQuerier,
	intents []model.AuditWitnessIntent,
	witnesses []model.AuditWitness,
) (model.AuditHead, model.AuditAnchor, auditMutationMetrics, error) {
	observed := &observedAuditStateQuerier{auditStateQuerier: querier}
	pendingPublications, err := readPendingPublications(ctx, observed)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, observed.metrics(), err
	}
	pendingPublicationIDs := make(map[string]struct{}, len(pendingPublications))
	for _, publication := range pendingPublications {
		pendingPublicationIDs[publication.ID] = struct{}{}
	}
	snapshot, err := readAuditMutationSnapshot(ctx, observed, pendingPublicationIDs)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, observed.metrics(), err
	}
	if err := verifyPendingPublicationSnapshot(pendingPublications, snapshot.latestPublicationActions); err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, observed.metrics(), err
	}
	if err := verifyAuditWitnessIntentsAgainstIndex(intents, snapshot.byID); err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, observed.metrics(), err
	}
	witnessedHeads, err := verifyAuditWitnessesAgainstIndex(witnesses, snapshot.byID)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, observed.metrics(), err
	}
	anchor, err := verifyAuditMutationAnchors(ctx, observed, snapshot, witnessedHeads)
	if err != nil {
		return model.AuditHead{}, model.AuditAnchor{}, observed.metrics(), err
	}
	return snapshot.head, anchor, observed.metrics(), nil
}

func readAuditMutationSnapshot(
	ctx context.Context,
	querier auditStateQuerier,
	pendingPublicationIDs map[string]struct{},
) (auditMutationSnapshot, error) {
	snapshot := auditMutationSnapshot{
		head: model.AuditHead{SHA256: append([]byte(nil), auditGenesisSHA256...)},
		byID: map[int64][sha256.Size]byte{0: {}}, latestPublicationActions: make(map[string]auditPublicationAction),
	}
	head, err := walkVerifiedAuditChain(ctx, querier, func(action model.AuditAction) {
		digest := [sha256.Size]byte(action.SHA256)
		snapshot.byID[action.ID] = digest
		if action.ResourceType == "publication" {
			if _, pending := pendingPublicationIDs[action.ResourceID]; !pending {
				return
			}
			snapshot.latestPublicationActions[action.ResourceID] = auditPublicationAction{
				actor: action.Actor, action: action.Action, details: action.Details,
			}
		}
	})
	if err != nil {
		return auditMutationSnapshot{}, err
	}
	snapshot.head = head
	return snapshot, nil
}

func readPendingPublications(
	ctx context.Context,
	querier auditStateQuerier,
) ([]model.Publication, error) {
	rows, err := querier.QueryContext(ctx, `
SELECT `+publicationColumns+` FROM publications
WHERE status = 'pending' ORDER BY sequence`)
	if err != nil {
		return nil, fmt.Errorf("verify pending publication intents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var pending []model.Publication
	for rows.Next() {
		publication, scanErr := scanPublication(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("verify pending publication intent row: %w", scanErr)
		}
		pending = append(pending, publication)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("verify pending publication intents: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close pending publication intents: %w", err)
	}
	return pending, nil
}

func verifyPendingPublicationSnapshot(pending []model.Publication, latest map[string]auditPublicationAction) error {
	for _, publication := range pending {
		action, ok := latest[publication.ID]
		if !ok {
			return fmt.Errorf("pending publication %s has no chained audit intent", publication.ID)
		}
		if action.actor != publication.Actor || (action.action != "publication.pending" && action.action != "publication.predecessor") {
			return fmt.Errorf("pending publication %s does not match its latest chained audit action", publication.ID)
		}
		expected, err := publicationAuditDetails(publication, "")
		if err != nil {
			return err
		}
		if action.details != expected {
			return fmt.Errorf("pending publication %s differs from its chained audit intent", publication.ID)
		}
	}
	return nil
}

func verifyAuditWitnessIntentsAgainstIndex(
	intents []model.AuditWitnessIntent,
	byID map[int64][sha256.Size]byte,
) error {
	for index, intent := range intents {
		if intent.Sequence != index+1 || intent.AuditID < 0 || len(intent.AuditSHA256) != sha256.Size {
			return fmt.Errorf("%w: external audit witness intent %d is invalid", ErrInvalid, index+1)
		}
		if index > 0 && intent.AuditID <= intents[index-1].AuditID {
			return fmt.Errorf("%w: external audit witness intent %d does not advance the audit sequence", ErrInvalid, index+1)
		}
		digest := [sha256.Size]byte(intent.AuditSHA256)
		stored, ok := byID[intent.AuditID]
		if !ok || stored != digest {
			return fmt.Errorf("external audit witness intent %d references audit action %d absent from the local chain", intent.Sequence, intent.AuditID)
		}
	}
	return nil
}

func verifyAuditWitnessesAgainstIndex(
	witnesses []model.AuditWitness,
	byID map[int64][sha256.Size]byte,
) (map[[sha256.Size]byte]struct{}, error) {
	seen := make(map[string]bool, len(witnesses))
	witnessedHeads := make(map[[sha256.Size]byte]struct{}, len(witnesses))
	for _, witness := range witnesses {
		if strings.TrimSpace(witness.UUID) == "" || witness.LogIndex < 0 || witness.IntegratedAt.IsZero() ||
			witness.AuditID < 0 || len(witness.AuditSHA256) != sha256.Size {
			return nil, fmt.Errorf("%w: external audit witness fields are invalid", ErrInvalid)
		}
		if seen[witness.UUID] {
			return nil, fmt.Errorf("%w: duplicate external audit witness %s", ErrInvalid, witness.UUID)
		}
		seen[witness.UUID] = true
		digest := [sha256.Size]byte(witness.AuditSHA256)
		stored, ok := byID[witness.AuditID]
		if !ok || stored != digest {
			return nil, fmt.Errorf("external audit witness %s references audit action %d absent from the local chain", witness.UUID, witness.AuditID)
		}
		witnessedHeads[digest] = struct{}{}
	}
	return witnessedHeads, nil
}

func verifyAuditMutationAnchors(
	ctx context.Context,
	querier auditStateQuerier,
	snapshot auditMutationSnapshot,
	witnessedHeads map[[sha256.Size]byte]struct{},
) (model.AuditAnchor, error) {
	rows, err := querier.QueryContext(ctx, `
SELECT id, audit_id, audit_sha256
FROM audit_anchors ORDER BY id`)
	if err != nil {
		return model.AuditAnchor{}, fmt.Errorf("read audit anchors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var latestID int64
	for rows.Next() {
		var anchorID, auditID int64
		var auditSHA256 []byte
		if scanErr := rows.Scan(&anchorID, &auditID, &auditSHA256); scanErr != nil {
			return model.AuditAnchor{}, fmt.Errorf("scan audit anchor: %w", scanErr)
		}
		if auditID < 0 || auditID > snapshot.head.ID || len(auditSHA256) != sha256.Size {
			return model.AuditAnchor{}, fmt.Errorf("audit anchor references invalid action %d", auditID)
		}
		digest := [sha256.Size]byte(auditSHA256)
		stored, ok := snapshot.byID[auditID]
		if !ok || stored != digest {
			return model.AuditAnchor{}, fmt.Errorf("audit anchor hash does not match action %d", auditID)
		}
		if _, ok := witnessedHeads[digest]; !ok {
			return model.AuditAnchor{}, fmt.Errorf("local audit anchor %d has no recoverable external witness", anchorID)
		}
		latestID = anchorID
	}
	if err := rows.Err(); err != nil {
		return model.AuditAnchor{}, fmt.Errorf("iterate audit anchors: %w", err)
	}
	if err := rows.Close(); err != nil {
		return model.AuditAnchor{}, fmt.Errorf("close audit anchors: %w", err)
	}
	if latestID == 0 {
		return model.AuditAnchor{}, errors.New("no persisted signed checkpoint remains")
	}
	latest, err := scanAuditAnchor(querier.QueryRowContext(ctx, `
SELECT id, audit_id, audit_sha256, tsa_url, receipt, anchored_at, created_at
FROM audit_anchors WHERE id = ?`, latestID))
	if err != nil {
		return model.AuditAnchor{}, fmt.Errorf("read latest audit anchor: %w", err)
	}
	return latest, nil
}

func verifyAuditChain(ctx context.Context, querier auditStateQuerier) (model.AuditHead, error) {
	return walkVerifiedAuditChain(ctx, querier, nil)
}

func walkVerifiedAuditChain(
	ctx context.Context,
	querier auditStateQuerier,
	visit func(model.AuditAction),
) (model.AuditHead, error) {
	if observer, ok := querier.(interface{ recordFullAuditTraversal() }); ok {
		observer.recordFullAuditTraversal()
	}
	rows, err := querier.QueryContext(ctx, `
SELECT id, actor, action, resource_type, resource_id, details, previous_sha256, sha256, created_at
FROM audit_actions ORDER BY id`)
	if err != nil {
		return model.AuditHead{}, fmt.Errorf("read audit chain: %w", err)
	}
	defer func() { _ = rows.Close() }()
	expectedID := int64(1)
	previous := append([]byte(nil), auditGenesisSHA256...)
	head := model.AuditHead{SHA256: append([]byte(nil), auditGenesisSHA256...)}
	for rows.Next() {
		var action model.AuditAction
		var createdAt int64
		if err := rows.Scan(&action.ID, &action.Actor, &action.Action, &action.ResourceType, &action.ResourceID,
			&action.Details, &action.PreviousSHA256, &action.SHA256, &createdAt); err != nil {
			return model.AuditHead{}, fmt.Errorf("scan audit action: %w", err)
		}
		if action.ID != expectedID {
			return model.AuditHead{}, fmt.Errorf("audit chain sequence is broken at id %d, expected %d", action.ID, expectedID)
		}
		if !bytes.Equal(action.PreviousSHA256, previous) {
			return model.AuditHead{}, fmt.Errorf("audit action %d predecessor hash is invalid", action.ID)
		}
		spec := model.AuditActionSpec{Actor: action.Actor, Action: action.Action, ResourceType: action.ResourceType, ResourceID: action.ResourceID, Details: action.Details}
		expected := auditActionSHA256(action.ID, previous, spec, createdAt)
		if !bytes.Equal(action.SHA256, expected) {
			return model.AuditHead{}, fmt.Errorf("audit action %d hash is invalid", action.ID)
		}
		if visit != nil {
			visit(action)
		}
		previous = append(previous[:0], action.SHA256...)
		head = model.AuditHead{ID: action.ID, SHA256: append([]byte(nil), action.SHA256...)}
		expectedID++
	}
	if err := rows.Err(); err != nil {
		return model.AuditHead{}, fmt.Errorf("iterate audit chain: %w", err)
	}
	return head, nil
}

func (s *Store) RecordAuditAnchor(ctx context.Context, spec model.AuditAnchorSpec) (model.AuditAnchor, error) {
	if spec.AuditID < 0 || len(spec.AuditSHA256) != sha256.Size || strings.TrimSpace(spec.TSAURL) == "" ||
		len(spec.Receipt) == 0 || len(spec.Receipt) > maxAuditReceiptBytes || spec.AnchoredAt.IsZero() {
		return model.AuditAnchor{}, fmt.Errorf("%w: audit anchor fields are invalid", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AuditAnchor{}, fmt.Errorf("begin audit anchor: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	storedHash := append([]byte(nil), auditGenesisSHA256...)
	if spec.AuditID > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT sha256 FROM audit_actions WHERE id = ?`, spec.AuditID).Scan(&storedHash); err != nil {
			return model.AuditAnchor{}, translateNotFound("audit action", err)
		}
	}
	if !bytes.Equal(storedHash, spec.AuditSHA256) {
		return model.AuditAnchor{}, fmt.Errorf("%w: anchor does not match audit action %d", ErrInvalidState, spec.AuditID)
	}
	createdAt := unixNano(s.now())
	result, err := tx.ExecContext(ctx, `
INSERT INTO audit_anchors(audit_id, audit_sha256, tsa_url, receipt, anchored_at, created_at)
VALUES(?, ?, ?, ?, ?, ?)`, spec.AuditID, append([]byte(nil), spec.AuditSHA256...),
		spec.TSAURL, append([]byte(nil), spec.Receipt...), unixNano(spec.AnchoredAt), createdAt)
	if err != nil {
		return model.AuditAnchor{}, fmt.Errorf("record audit anchor: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.AuditAnchor{}, fmt.Errorf("read audit anchor id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.AuditAnchor{}, fmt.Errorf("commit audit anchor: %w", err)
	}
	return s.auditAnchor(ctx, id)
}

func (s *Store) LatestAuditAnchor(ctx context.Context) (model.AuditAnchor, error) {
	return scanAuditAnchor(s.db.QueryRowContext(ctx, `
SELECT id, audit_id, audit_sha256, tsa_url, receipt, anchored_at, created_at
FROM audit_anchors ORDER BY id DESC LIMIT 1`))
}

func (s *Store) AuditAnchors(ctx context.Context) ([]model.AuditAnchor, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, audit_id, audit_sha256, tsa_url, receipt, anchored_at, created_at
FROM audit_anchors ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list audit anchors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var anchors []model.AuditAnchor
	for rows.Next() {
		anchor, err := scanAuditAnchor(rows)
		if err != nil {
			return nil, fmt.Errorf("scan audit anchor: %w", err)
		}
		anchors = append(anchors, anchor)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit anchors: %w", err)
	}
	return anchors, nil
}

func (s *Store) VerifyAuditWitnesses(ctx context.Context, witnesses []model.AuditWitness) error {
	if _, err := s.VerifyAuditChain(ctx); err != nil {
		return err
	}
	byID, err := s.auditHashIndex(ctx)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(witnesses))
	witnessedHeads := make(map[[sha256.Size]byte]struct{}, len(witnesses))
	for _, witness := range witnesses {
		if strings.TrimSpace(witness.UUID) == "" || witness.LogIndex < 0 || witness.IntegratedAt.IsZero() ||
			witness.AuditID < 0 || len(witness.AuditSHA256) != sha256.Size {
			return fmt.Errorf("%w: external audit witness fields are invalid", ErrInvalid)
		}
		if seen[witness.UUID] {
			return fmt.Errorf("%w: duplicate external audit witness %s", ErrInvalid, witness.UUID)
		}
		seen[witness.UUID] = true
		digest := [sha256.Size]byte(witness.AuditSHA256)
		stored, ok := byID[witness.AuditID]
		if !ok || stored != digest {
			return fmt.Errorf("external audit witness %s references audit action %d absent from the local chain", witness.UUID, witness.AuditID)
		}
		witnessedHeads[digest] = struct{}{}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, audit_sha256 FROM audit_anchors ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read audit anchor references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var value []byte
		if err := rows.Scan(&id, &value); err != nil {
			return fmt.Errorf("scan audit anchor reference: %w", err)
		}
		if len(value) != sha256.Size {
			return fmt.Errorf("local audit anchor %d has an invalid hash length", id)
		}
		digest := [sha256.Size]byte(value)
		if _, ok := witnessedHeads[digest]; !ok {
			return fmt.Errorf("local audit anchor %d has no recoverable external witness", id)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate audit anchor references: %w", err)
	}
	return nil
}

// VerifyAuditWitnessIntents ensures every rollback-external pre-publication
// intent still names an exact head in the current local audit chain. This check
// runs before Rekor recovery, so a process crash followed by a whole-PVC
// rollback cannot hide an externally visible publication attempt.
func (s *Store) VerifyAuditWitnessIntents(ctx context.Context, intents []model.AuditWitnessIntent) error {
	if _, err := s.VerifyAuditChain(ctx); err != nil {
		return err
	}
	byID, err := s.auditHashIndex(ctx)
	if err != nil {
		return err
	}
	for index, intent := range intents {
		if intent.Sequence != index+1 || intent.AuditID < 0 || len(intent.AuditSHA256) != sha256.Size {
			return fmt.Errorf("%w: external audit witness intent %d is invalid", ErrInvalid, index+1)
		}
		if index > 0 && intent.AuditID <= intents[index-1].AuditID {
			return fmt.Errorf("%w: external audit witness intent %d does not advance the audit sequence", ErrInvalid, index+1)
		}
		digest := [sha256.Size]byte(intent.AuditSHA256)
		stored, ok := byID[intent.AuditID]
		if !ok || stored != digest {
			return fmt.Errorf("external audit witness intent %d references audit action %d absent from the local chain", intent.Sequence, intent.AuditID)
		}
	}
	return nil
}

func (s *Store) auditHashIndex(ctx context.Context) (map[int64][sha256.Size]byte, error) {
	byID := make(map[int64][sha256.Size]byte)
	byID[0] = [sha256.Size]byte{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, sha256 FROM audit_actions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read audit hash index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var value []byte
		if err := rows.Scan(&id, &value); err != nil {
			return nil, fmt.Errorf("scan audit hash index: %w", err)
		}
		if len(value) != sha256.Size {
			return nil, fmt.Errorf("audit action %d has an invalid hash length", id)
		}
		digest := [sha256.Size]byte(value)
		byID[id] = digest
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit hash index: %w", err)
	}
	return byID, nil
}

func (s *Store) VerifyAuditAnchorReference(ctx context.Context, anchor model.AuditAnchor) error {
	return s.VerifyAuditAnchorReferences(ctx, []model.AuditAnchor{anchor})
}

func (s *Store) VerifyAuditAnchorReferences(ctx context.Context, anchors []model.AuditAnchor) error {
	head, err := s.VerifyAuditChain(ctx)
	if err != nil {
		return err
	}
	byID, err := s.auditHashIndex(ctx)
	if err != nil {
		return err
	}
	for _, anchor := range anchors {
		if anchor.AuditID < 0 || anchor.AuditID > head.ID || len(anchor.AuditSHA256) != sha256.Size {
			return fmt.Errorf("audit anchor references invalid action %d", anchor.AuditID)
		}
		storedHash, exists := byID[anchor.AuditID]
		if !exists || !bytes.Equal(storedHash[:], anchor.AuditSHA256) {
			return fmt.Errorf("audit anchor hash does not match action %d", anchor.AuditID)
		}
	}
	return nil
}

func (s *Store) auditAnchor(ctx context.Context, id int64) (model.AuditAnchor, error) {
	return scanAuditAnchor(s.db.QueryRowContext(ctx, `
SELECT id, audit_id, audit_sha256, tsa_url, receipt, anchored_at, created_at
FROM audit_anchors WHERE id = ?`, id))
}

func scanAuditAnchor(row rowScanner) (model.AuditAnchor, error) {
	var value model.AuditAnchor
	var anchoredAt, createdAt int64
	if err := row.Scan(&value.ID, &value.AuditID, &value.AuditSHA256, &value.TSAURL, &value.Receipt, &anchoredAt, &createdAt); err != nil {
		return model.AuditAnchor{}, translateNotFound("audit anchor", err)
	}
	value.AnchoredAt, value.CreatedAt = fromUnixNano(anchoredAt), fromUnixNano(createdAt)
	return value, nil
}
