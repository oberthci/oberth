package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestAdmissionHoldSystemStatusAndScopedOmission(t *testing.T) {
	hold := model.AdmissionHold{On: true, Reason: "release qualification", Actor: "admin", Allow: []string{"tag", "release"}, ExpiresAt: time.Now().Add(time.Hour)}
	db, err := storetest.Open(t.Context(), filepath.Join(t.TempDir(), "status.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	health := Health{Store: db, Admission: func() *model.AdmissionState { return &model.AdmissionState{Hold: &hold} }}
	control, err := service.NewAPI(service.APIConfig{Runs: db, Health: health})
	if err != nil {
		t.Fatal(err)
	}
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "admin", Admin: true}, "system_status", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil || !strings.Contains(string(raw), `"hold":{"on":true`) || !strings.Contains(string(raw), hold.ExpiresAt.Format(time.RFC3339Nano)) {
		t.Fatal(string(raw), err)
	}
	scoped := scopedAdmission(health.Admission(), nil, nil, api.Actor{Scope: []string{"visible"}})
	if scoped.Hold != nil {
		t.Fatal("scoped status exposed installation configuration", scoped)
	}
}
