package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestSourceRequestDatabaseOutageRecordsItsVerifiedPod(t *testing.T) {
	f := newDeliveryFixture(t)
	if err := f.o.c.store.Close(); err != nil {
		t.Fatal(err)
	}
	c := sourcedelivery.Config{SourceClientBurst: 3, Authenticate: f.authentication(t).Authenticate, Run: f.o.sourceRun, Stage: func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
		t.Error("failed run read reached source staging")
		return sourcedelivery.Staged{}, nil
	}, Now: f.o.c.clock.Now, RecordFailure: func(ctx context.Context, who sourcedelivery.Caller) error {
		return f.o.recordDeliveryFailure(ctx, who, "source")
	}}
	body, err := json.Marshal(sourcedelivery.LoadRequest{Locator: f.run.ID, Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), "POST", sourcedelivery.LoadPath, strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+f.jwt)
	w := httptest.NewRecorder()
	sourcedelivery.New(c).ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("database failure was not unavailable", w.Code)
	}
	receipts, err := f.o.c.logs.Infrastructure(t.Context(), f.run.ID)
	if err != nil || len(receipts) != 1 || receipts[0].Kind != "source" || receipts[0].PodUID != string(f.pod.UID) {
		t.Fatal("source receipt lost verified identity", receipts, err)
	}
	f.assertDiagnosis(t, receipts)
}
