package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// ScheduleAuditResourceID names a schedule entry in the audit chain. The
// qualified repository name never contains '#', so the entry name is whatever
// follows the first one.
func ScheduleAuditResourceID(qualifiedRepo, entry string) string {
	return qualifiedRepo + "#" + entry
}

// SchedulePauses returns the paused entries of one repository, keyed by entry
// name. A schedule.pause action pauses an entry until a later schedule.resume
// for the same entry; the audit chain is the record of both. The scan reads
// only schedule actions, newest state winning, and callers ask only when an
// entry is due or an operator lists schedules.
func (s *Store) SchedulePauses(ctx context.Context, qualifiedRepo string) (map[string]model.SchedulePause, error) {
	prefix := qualifiedRepo + "#"
	rows, err := s.reader().QueryContext(ctx, `
SELECT action, actor, resource_id, details, created_at FROM audit_actions
WHERE resource_type = 'schedule' AND action IN ('schedule.pause', 'schedule.resume')
  AND substr(resource_id, 1, ?) = ?
ORDER BY id`, len(prefix), prefix)
	if err != nil {
		return nil, fmt.Errorf("read schedule pauses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pauses := map[string]model.SchedulePause{}
	for rows.Next() {
		var action, actor, resourceID, details string
		var createdAt int64
		if err := rows.Scan(&action, &actor, &resourceID, &details, &createdAt); err != nil {
			return nil, fmt.Errorf("scan schedule pause: %w", err)
		}
		entry := strings.TrimPrefix(resourceID, prefix)
		if action == "schedule.resume" {
			delete(pauses, entry)
			continue
		}
		var parsed struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal([]byte(details), &parsed)
		pauses[entry] = model.SchedulePause{By: actor, At: fromUnixNano(createdAt), Reason: parsed.Reason}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schedule pauses: %w", err)
	}
	return pauses, nil
}
