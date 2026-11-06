package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type capHealthStore struct{ fakeHealthStore }

func (capHealthStore) QueueCap(context.Context) (model.QueueCapStatus, error) {
	return model.QueueCapStatus{MaxNotStarted: 3, NotStarted: 4, Deferred: 2, Reason: "branch run deferred: queue at 4/3"}, nil
}

func TestHealthReportsQueueCapOnlyForUnscopedStatus(t *testing.T) {
	health := Health{Store: capHealthStore{}}
	value, err := health.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Cap model.QueueCapStatus `json:"queueCap"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cap.MaxNotStarted != 3 || got.Cap.NotStarted != 4 || got.Cap.Deferred != 2 {
		t.Fatalf("queue cap missing from system_status: %s", body)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	var canonical map[string]any
	if err := json.Unmarshal(fields["queue_cap"], &canonical); err != nil {
		t.Fatal(err)
	}
	if len(canonical) != 4 || canonical["max_not_started"] != float64(3) || canonical["not_started"] != float64(4) || canonical["deferred"] != float64(2) {
		t.Fatalf("canonical queue_cap: %s", body)
	}
	value, err = health.ScopedStatus(t.Context(), api.Actor{Identity: "scoped", Scope: []string{"codeberg"}})
	if err != nil {
		t.Fatal(err)
	}
	body, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	fields = nil
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["queueCap"] != nil || fields["queue_cap"] != nil {
		t.Fatalf("scoped health exposes global queue cap: %s", body)
	}
}
