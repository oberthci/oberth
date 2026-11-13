package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type metricsHealthStore struct{ fakeHealthStore }

func (metricsHealthStore) LastBackup(context.Context) (*model.BackupStatus, error) {
	return &model.BackupStatus{At: "2026-11-10T03:00:00.5Z"}, nil
}

func (metricsHealthStore) AdminTokenExpiry(context.Context) (*model.AdminTokenExpiry, error) {
	return &model.AdminTokenExpiry{EarliestExpiresAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

func TestWriteStatusMetricsExportsOperatorGauges(t *testing.T) {
	health := Health{
		Store:             metricsHealthStore{},
		ControllerProfile: func() *model.ControllerProfileStatus { return &model.ControllerProfileStatus{DispatchBlocked: true} },
		Storage: func(context.Context) (any, error) {
			return map[string]any{"database_bytes": 1 << 20, "free_bytes": 5000, "below_floor": false}, nil
		},
		AuditChain: func(context.Context) (AuditChainStatus, error) {
			return AuditChainStatus{AnchoredAt: "2026-11-10T04:00:00Z"}, nil
		},
	}
	var out strings.Builder
	health.WriteStatusMetrics(t.Context(), &out)
	for _, want := range []string{
		"oberth_controller_verification_blocked 1\n",
		"oberth_database_bytes 1048576\n",
		"oberth_data_free_bytes 5000\n",
		fmt.Sprintf("oberth_audit_anchor_timestamp_seconds %d\n", time.Date(2026, 11, 10, 4, 0, 0, 0, time.UTC).Unix()),
		fmt.Sprintf("oberth_last_backup_timestamp_seconds %d\n", time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC).Unix()),
		fmt.Sprintf("oberth_admin_token_earliest_expiry_seconds %d\n", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).Unix()),
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
	// A failing source is left out, never reported as zero.
	failing := Health{Storage: func(context.Context) (any, error) { return nil, errors.New("unavailable") }}
	out.Reset()
	failing.WriteStatusMetrics(t.Context(), &out)
	if out.Len() != 0 {
		t.Fatalf("failing sources exported %q", out.String())
	}
}
