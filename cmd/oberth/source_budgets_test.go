package main

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestRuntimeSourceTupleReachesStatusAndRejectsExplicitZero(t *testing.T) {
	for _, b := range []*sourcedelivery.Budgets{nil, {}, {Exports: 2, Archives: 4, Transfers: 4, Auth: 32, WorkflowWaiters: 16}} {
		var fields map[string]any
		if err := json.Unmarshal(validRuntime(), &fields); err != nil {
			t.Fatal(err)
		}
		if b != nil {
			fields["source_delivery"] = b
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		r, err := decodeRuntime(raw)
		if b != nil && b.Exports == 0 {
			if err == nil {
				t.Fatal("explicit zero accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := "exports=1 archives=2 transfers=2 auth=32 workflow_waiters=16"
		if b != nil {
			want = "exports=2 archives=4 transfers=4 auth=32 workflow_waiters=16"
		}
		checkSourceStatusSetting(t, r, want)
	}
}

func checkSourceStatusSetting(t *testing.T, r serverRuntime, want string) {
	t.Helper()
	rows := settingsRows{}
	rows.server(serverConfig{}, r)
	for _, row := range rows.values {
		if row.Name == "source delivery" {
			if row.Value != want {
				t.Fatal("effective source tuple differs", row.Value, want)
			}
			return
		}
	}
	t.Fatal("effective tuple missing from status", rows.values)
}
