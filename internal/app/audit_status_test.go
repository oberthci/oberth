package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

type backupStatusAuth struct{ actor api.Actor }

func (auth backupStatusAuth) Authenticate(context.Context, string) (api.Actor, error) {
	return auth.actor, nil
}

func TestBackupStatusMCPAndHTTPReadAuditRecord(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "status.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	health := Health{Store: database}
	control, err := service.NewAPI(service.APIConfig{Runs: database, Health: health})
	if err != nil {
		t.Fatal(err)
	}
	check := func(actor api.Actor, want *model.BackupStatus) {
		t.Helper()
		server, err := api.New(backupStatusAuth{actor}, control, control, "test")
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		request.Header.Set("Authorization", "Bearer test")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		var status HealthStatus
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || response.Code != http.StatusOK {
			t.Fatalf("HTTP %d %s: %v", response.Code, response.Body, err)
		}
		value, err := control.CallTool(ctx, actor, "system_status", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		mcp := value.(HealthStatus)
		if !reflect.DeepEqual(status.LastBackup, want) || !reflect.DeepEqual(mcp.LastBackup, want) {
			t.Fatalf("HTTP=%+v MCP=%+v want=%+v", status.LastBackup, mcp.LastBackup, want)
		}
	}
	actor := api.Actor{Identity: "admin", Admin: true}
	check(actor, nil)
	action, err := database.RecordOperationAudit(ctx, "admin", "backup.completed", `{"archive_sha256":"archive","audit_head_id":42,"audit_head_sha256":"snapshot"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordOperationAudit(ctx, "admin", "restore.completed", `{}`); err != nil {
		t.Fatal(err)
	}
	check(actor, &model.BackupStatus{At: action.CreatedAt.UTC().Format(time.RFC3339Nano), Head: model.BackupAuditHead{ID: 42, SHA256: "snapshot"}, ArchiveSHA256: "archive"})
	check(api.Actor{Identity: "scoped-admin", Admin: true, Scope: []string{"github"}}, nil)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	value, err := health.Status(ctx)
	if err != nil || value.(HealthStatus).Database != "unavailable" {
		t.Fatalf("unavailable database status=%+v err=%v", value, err)
	}
}

func TestUnreadableBackupRecordIsABackupWarningNotADatabaseOutage(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "status.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	// Valid JSON, so the chain accepts it, but not a backup record.
	action, err := database.RecordOperationAudit(ctx, "admin", "backup.completed", `{"archive_sha256":"archive","audit_head_id":"forty-two"}`)
	if err != nil {
		t.Fatal(err)
	}
	value, err := Health{Store: database}.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.Database != "ready" || status.LastBackup != nil {
		t.Fatalf("database=%q last_backup=%+v; want a ready database and no last_backup", status.Database, status.LastBackup)
	}
	if !strings.Contains(status.LastBackupWarning, model.ErrBackupRecordUnreadable.Error()) || !strings.Contains(status.LastBackupWarning, strconv.FormatInt(action.ID, 10)) {
		t.Fatalf("last_backup_warning = %q, want the unreadable record named", status.LastBackupWarning)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"last_backup_warning":`) || strings.Contains(string(encoded), `"last_backup":`) {
		t.Fatalf("status JSON = %s", encoded)
	}
}
