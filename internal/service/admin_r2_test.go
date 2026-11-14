package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestAdministrationAuditDetailsBounded(t *testing.T) {
	_, _, handler := newAdministrationFixture(t)
	for action, keys := range lifecycleAuditKeys {
		for key := range keys {
			body, _ := json.Marshal(map[string]any{"action": action, "details": map[string]any{key: "public fact"}})
			if out := adminRequest(handler, "audit-record", "admin", string(body)); out.Code != http.StatusOK {
				t.Fatal(action, key, out.Code, out.Body)
			}
		}
	}
	for _, details := range []string{`{"token":"credential"}`, `{"from":{"nested":true}}`, `{"to":[]}`, `{"rollback":null}`, `[]`, `null`} {
		if out := adminRequest(handler, "audit-record", "admin", `{"action":"upgrade.completed","details":`+details+`}`); out.Code == http.StatusOK {
			t.Fatal("unsafe lifecycle details accepted", details)
		}
	}
	for _, size := range []int{adminAuditDetailsBytes, adminAuditDetailsBytes + 1} {
		details := `{"to":"` + strings.Repeat("x", size-len(`{"to":""}`)) + `"}`
		out := adminRequest(handler, "audit-record", "admin", `{"action":"upgrade.completed","details":`+details+`}`)
		if (size == adminAuditDetailsBytes) != (out.Code == http.StatusOK) {
			t.Fatal("incorrect details bound", size, out.Code, out.Body)
		}
	}
}

type trackedAdminArchive struct {
	io.Reader
	closed atomic.Bool
}

func (a *trackedAdminArchive) Close() error { a.closed.Store(true); return nil }

func TestAdministrationBackupSingleFlight(t *testing.T) {
	control, _, handler := newAdministrationFixture(t)
	var calls int
	var opened []*trackedAdminArchive
	control.administration.DatabaseBackup = func(context.Context, int64) (io.ReadCloser, error) {
		calls++
		stream := &trackedAdminArchive{Reader: strings.NewReader("verified bytes")}
		opened = append(opened, stream)
		return stream, nil
	}
	held, err := control.PrepareAdminBackup(t.Context(), api.Actor{Identity: "owner", Admin: true}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	clone := *control
	if another, err := clone.PrepareAdminBackup(t.Context(), api.Actor{Identity: "owner", Admin: true}, json.RawMessage(`{}`)); !errors.Is(err, api.ErrAdminBackupBusy) {
		if another != nil {
			_ = another.Close()
		}
		t.Fatal("scope copy bypassed single flight", err)
	}
	out := adminRequest(handler, "database-backup", "admin", `{}`)
	if out.Code != http.StatusTooManyRequests || calls != 1 {
		t.Fatal("second download started", out.Code, calls, out.Body)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if !opened[0].closed.Load() {
		t.Fatal("snapshot not closed")
	}
	out = adminRequest(handler, "database-backup", "admin", `{}`)
	if out.Code != http.StatusOK || calls != 2 || !opened[1].closed.Load() {
		t.Fatal("slot not released or response not closed", out.Code, calls)
	}
}
