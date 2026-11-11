package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// WriteStatusMetrics exports the system_status values an operator alerts on:
// queue pressure, a paused admission, backup and audit-anchor age, admin token
// expiry and the database size the backup ceiling is measured against. Every
// value is read from the same sources as Status; a source that fails is left
// out rather than reported as zero. No probe here touches the network.
func (health Health) WriteStatusMetrics(ctx context.Context, w io.Writer) {
	gauge := func(name, help string, value float64) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, strconv.FormatFloat(value, 'f', -1, 64))
	}
	flag := func(value bool) float64 {
		if value {
			return 1
		}
		return 0
	}
	if health.ControllerProfile != nil {
		if profile := health.ControllerProfile(); profile != nil {
			gauge("oberth_admission_paused", "1 while the controller profile check pauses dispatch", flag(profile.AdmissionPaused))
		}
	}
	if health.Storage != nil {
		if raw, err := health.Storage(ctx); err == nil {
			var storage struct {
				DatabaseBytes *int64 `json:"database_bytes"`
				FreeBytes     *int64 `json:"free_bytes"`
				BelowFloor    *bool  `json:"below_floor"`
			}
			if body, err := json.Marshal(raw); err == nil && json.Unmarshal(body, &storage) == nil {
				if storage.DatabaseBytes != nil && *storage.DatabaseBytes > 0 {
					gauge("oberth_database_bytes", "live database plus write-ahead log at the last retention sweep", float64(*storage.DatabaseBytes))
				}
				if storage.FreeBytes != nil {
					gauge("oberth_data_free_bytes", "free bytes on the data volume at the last retention sweep", float64(*storage.FreeBytes))
				}
				if storage.BelowFloor != nil {
					gauge("oberth_data_below_disk_floor", "1 while free space is below the admission disk floor", flag(*storage.BelowFloor))
				}
			}
		}
	}
	if health.AuditChain != nil {
		if chain, err := health.AuditChain(ctx); err == nil && chain.AnchoredAt != "" {
			if at, err := time.Parse(time.RFC3339, chain.AnchoredAt); err == nil {
				gauge("oberth_audit_anchor_timestamp_seconds", "time of the latest external audit checkpoint", float64(at.Unix()))
			}
		}
	}
	if health.Store == nil {
		return
	}
	if reader, ok := health.Store.(interface {
		QueueCap(context.Context) (model.QueueCapStatus, error)
	}); ok {
		if queue, err := reader.QueueCap(ctx); err == nil {
			gauge("oberth_queue_not_started", "runs queued or waiting for admission", float64(queue.NotStarted))
			gauge("oberth_queue_max_not_started", "configured bound on not-started runs (0 is unbounded)", float64(queue.MaxNotStarted))
			gauge("oberth_queue_deferred", "pushes deferred by the not-started bound", float64(queue.Deferred))
		}
	}
	if reader, ok := health.Store.(interface {
		LastBackup(context.Context) (*model.BackupStatus, error)
	}); ok {
		if backup, err := reader.LastBackup(ctx); err == nil && backup != nil {
			if at, err := time.Parse(time.RFC3339Nano, backup.At); err == nil {
				gauge("oberth_last_backup_timestamp_seconds", "time of the latest recorded backup", float64(at.Unix()))
			}
		}
	}
	if reader, ok := health.Store.(interface {
		AdminTokenExpiry(context.Context) (*model.AdminTokenExpiry, error)
	}); ok {
		if expiry, err := reader.AdminTokenExpiry(ctx); err == nil && expiry != nil && !expiry.EarliestExpiresAt.IsZero() {
			gauge("oberth_admin_token_earliest_expiry_seconds", "earliest expiry among active admin tokens", float64(expiry.EarliestExpiresAt.Unix()))
		}
	}
}
