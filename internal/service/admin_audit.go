package service

import (
	"bytes"
	"encoding/json"
)

const adminAuditDetailsBytes = 2 << 10

var lifecycleAuditKeys = map[string]map[string]bool{
	"backup.completed":  {"archive_sha256": true, "bytes": true, "scope": true, "audit_head_id": true, "audit_head_sha256": true},
	"restore.completed": {"archive_sha256": true, "history": true, "secrets": true, "from_audit_head_id": true, "from_audit_head_sha256": true},
	"upgrade.completed": {"from": true, "to": true, "rollback": true},
}

func validateLifecycleAuditDetails(action string, raw json.RawMessage) error {
	allowed, ok := lifecycleAuditKeys[action]
	if !ok {
		return refuse(ErrInvalidInput, "unknown lifecycle action")
	}
	if len(raw) > adminAuditDetailsBytes {
		return refuse(ErrInvalidInput, "lifecycle details exceed 2 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var details map[string]any
	if decoder.Decode(&details) != nil || details == nil {
		return refuse(ErrInvalidInput, "lifecycle details must be an object")
	}
	for key, value := range details {
		if !allowed[key] {
			return refuse(ErrInvalidInput, "unknown lifecycle detail")
		}
		switch value.(type) {
		case string, bool, json.Number:
		default:
			return refuse(ErrInvalidInput, "lifecycle details must be scalar values")
		}
	}
	return nil
}
