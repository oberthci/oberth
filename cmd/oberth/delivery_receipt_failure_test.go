package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestSecretDeliveryReceiptWriteFailureDoesNotChangeDelivery(t *testing.T) {
	for _, outage := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivered", true: "sealed"}[outage], func(t *testing.T) {
			f := newDeliveryFixture(t)
			var diagnostic bytes.Buffer
			f.o.c.diagnostic = &diagnostic
			f.backend.Sealed = outage
			refuseDeliveryReceiptWrites(t, f)
			reply := deliveryRequest(t, f.handler(t), f.jwt, `{"path":"registry/value"}`)
			want := 200
			if outage {
				want = 503
			}
			if reply.Code != want {
				t.Fatal("receipt storage changed delivery outcome", reply.Code)
			}
			if !outage && !strings.Contains(reply.Body.String(), "cHJpdmF0ZS1maXh0dXJl") {
				t.Fatal("delivery lost its value")
			}
			assertReceiptFailureLogged(t, f, diagnostic.String(), outage)
		})
	}
}

func refuseDeliveryReceiptWrites(t *testing.T, f deliveryFixture) {
	t.Helper()
	if err := os.Chmod(f.logDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.logDir, 0700) })
}

func assertReceiptFailureLogged(t *testing.T, f deliveryFixture, diagnostic string, failed bool) {
	t.Helper()
	if !strings.Contains(diagnostic, "level=ERROR") || !strings.Contains(diagnostic, "Delivery receipt write failed") || !strings.Contains(diagnostic, f.run.ID) || strings.Contains(diagnostic, f.jwt) || strings.Contains(diagnostic, "private-fixture") {
		t.Fatal("receipt failure was hidden or logged private material")
	}
	receipts, err := f.o.c.logs.Infrastructure(t.Context(), f.run.ID)
	if err != nil || !failed && len(receipts) != 0 || failed && (len(receipts) != 1 || !receipts[0].Uncertain) {
		t.Fatal("failed receipt write claimed certain evidence", receipts, err)
	}
}

func TestSourceDeliveryReceiptWriteFailureStillPublishesCompletion(t *testing.T) {
	f := newDeliveryFixture(t)
	var diagnostic bytes.Buffer
	f.o.c.diagnostic = &diagnostic
	refuseDeliveryReceiptWrites(t, f)
	c := sourcedelivery.Config{Budgets: &sourcedelivery.Budgets{Exports: 1, Archives: 2, Transfers: 2, Auth: 1, WorkflowWaiters: 16}, Now: f.o.c.clock.Now, Authenticate: f.authentication(t).Authenticate,
		Run: func(ctx context.Context, id string) (sourcedelivery.Run, error) {
			r, err := f.o.sourceRun(ctx, id)
			// The fixture's secret-only pipeline has no source declaration.
			r.Admission.SourceBindings = []sourcecontract.Binding{{Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}}}
			return r, err
		},
		Stage: func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
			return sourcedelivery.Staged{Body: io.NopCloser(strings.NewReader("source")), Bytes: 6}, nil
		}, RecordSuccess: func(ctx context.Context, caller sourcedelivery.Caller, started time.Time) error {
			return f.o.recordDelivery(ctx, caller, runlog.InfrastructureReceipt{Kind: "source", StartedAt: started, Delivered: true})
		}}
	body, err := json.Marshal(sourcedelivery.LoadRequest{Locator: f.run.ID, Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), "POST", sourcedelivery.LoadPath, strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+f.jwt)
	w := httptest.NewRecorder()
	sourcedelivery.New(c).ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "source" || w.Header().Get(sourcedelivery.ReceiptTrailer) == "" {
		t.Fatal("receipt write failure killed completed source delivery", w.Code, w.Header())
	}
	assertReceiptFailureLogged(t, f, diagnostic.String(), false)
}
