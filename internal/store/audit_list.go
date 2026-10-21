package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const (
	defaultAuditListLimit = 50
	maxAuditListLimit     = 200
)

// recordableAuditActions are the actions an in-pod operator command may append
// on behalf of a workstation operation the server cannot observe itself
// (backup, restore, upgrade). Anything else must be written by the code path
// that owns it.
var recordableAuditActions = map[string]bool{
	"backup.completed":  true,
	"restore.completed": true,
	"upgrade.completed": true,
}

// RecordOperationAudit appends one operator-reported lifecycle action
// (backup.completed, restore.completed, upgrade.completed) to the chain.
func (s *Store) RecordOperationAudit(ctx context.Context, actor, action, details string) (model.AuditAction, error) {
	if !recordableAuditActions[action] {
		return model.AuditAction{}, fmt.Errorf("%w: %q is not a recordable lifecycle action", ErrInvalid, action)
	}
	return s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: actor, Action: action, ResourceType: "system", ResourceID: "oberth", Details: details,
	})
}

// LastBackup reads the latest completion record, not the current chain head:
// the archive contains the snapshot's earlier head.
func (s *Store) LastBackup(ctx context.Context) (*model.BackupStatus, error) {
	action, err := s.LatestAuditActionByType(ctx, "backup.completed", "system")
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var details struct {
		ArchiveSHA256   string `json:"archive_sha256"`
		AuditHeadID     int64  `json:"audit_head_id"`
		AuditHeadSHA256 string `json:"audit_head_sha256"`
	}
	if err := json.Unmarshal([]byte(action.Details), &details); err != nil {
		return nil, fmt.Errorf("%w: audit action %d: %w", model.ErrBackupRecordUnreadable, action.ID, err)
	}
	return &model.BackupStatus{
		At:            action.CreatedAt.UTC().Format(time.RFC3339Nano),
		Head:          model.BackupAuditHead{ID: details.AuditHeadID, SHA256: details.AuditHeadSHA256},
		ArchiveSHA256: details.ArchiveSHA256,
	}, nil
}

// ListAuditActions returns audit actions newest first, each recomputed from
// its stored predecessor link and the configured audit domain, so a tampered
// row fails the call instead of being reported. It also returns the current
// head ID. Each entry carries its own ID, hash and predecessor hash, which is
// its chain position; verifying the whole chain remains VerifyAuditChain's job.
func (s *Store) ListAuditActions(ctx context.Context, filter model.AuditListFilter) ([]model.AuditAction, int64, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultAuditListLimit
	}
	if limit > maxAuditListLimit {
		limit = maxAuditListLimit
	}
	prefix := strings.TrimSpace(filter.ActionPrefix)
	actor := strings.TrimSpace(filter.Actor)
	var since, until int64
	if !filter.Since.IsZero() {
		since = unixNano(filter.Since)
	}
	if !filter.Until.IsZero() {
		until = unixNano(filter.Until)
	}
	byRepo := filter.RepoID > 0 || filter.RepoName != ""
	// One static statement: each filter is "unset OR matches", so no SQL is
	// assembled from caller input.
	const query = `
SELECT id, actor, action, resource_type, resource_id, details, previous_sha256, sha256, created_at FROM audit_actions
WHERE (? = 0 OR id < ?)
  AND (? = '' OR substr(action, 1, ?) = ?)
  AND (? = '' OR actor = ?)
  AND (? = 0 OR created_at >= ?)
  AND (? = 0 OR created_at < ?)
  AND (? = 0 OR
       (resource_type = 'repository' AND resource_id = ?)
       OR json_extract(details, '$.repo') = ?
       OR (? <> '' AND json_extract(details, '$.repo') = ?)
       OR json_extract(details, '$.repo_id') = ?
       OR (resource_type = 'run' AND resource_id IN (SELECT id FROM runs WHERE repo_id = ?))
       OR (resource_type = 'promotion' AND resource_id IN (SELECT id FROM promotions WHERE repo_id = ?)))
ORDER BY id DESC LIMIT ?`
	repoFlag := 0
	if byRepo {
		repoFlag = 1
	}
	args := []any{
		filter.Before, filter.Before,
		prefix, len(prefix), prefix,
		actor, actor,
		since, since,
		until, until,
		repoFlag, fmt.Sprint(filter.RepoID), filter.RepoName, filter.RepoQualifiedName, filter.RepoQualifiedName, filter.RepoID, filter.RepoID, filter.RepoID,
		limit,
	}
	// Read the head first: an action appended while the page is read is then
	// reported by the next poll instead of falling between the two.
	head, err := s.AuditHeadHint(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit actions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var actions []model.AuditAction
	for rows.Next() {
		var action model.AuditAction
		var createdAt int64
		if err := rows.Scan(&action.ID, &action.Actor, &action.Action, &action.ResourceType, &action.ResourceID,
			&action.Details, &action.PreviousSHA256, &action.SHA256, &createdAt); err != nil {
			return nil, 0, fmt.Errorf("scan audit action: %w", err)
		}
		spec := model.AuditActionSpec{Actor: action.Actor, Action: action.Action, ResourceType: action.ResourceType, ResourceID: action.ResourceID, Details: action.Details}
		if len(action.PreviousSHA256) != sha256.Size ||
			!bytes.Equal(action.SHA256, auditActionSHA256(action.ID, action.PreviousSHA256, spec, createdAt, s.protocol.AuditDomain)) {
			return nil, 0, fmt.Errorf("audit action %d hash is invalid", action.ID)
		}
		action.CreatedAt = fromUnixNano(createdAt)
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate audit actions: %w", err)
	}
	return actions, head.ID, nil
}
