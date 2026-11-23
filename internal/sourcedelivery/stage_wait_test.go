package sourcedelivery_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

type countedStage struct {
	io.Reader
	closed *atomic.Int32
}

func (r *countedStage) Close() error { r.closed.Add(1); return nil }

func TestExportWaitRechecksAuthorizationBeforeSending(t *testing.T) {
	for _, test := range []struct {
		name                      string
		waited, terminal, changed bool
		status, reads             int
	}{
		{"warm", false, true, false, 200, 1},
		{"waited", true, false, false, 200, 2},
		{"terminal", true, true, false, 403, 2},
		{"changed-admission", true, false, true, 403, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			const payload = "private archive bytes"
			config := deliveryRulesConfig()
			run := config.Run
			var staged atomic.Bool
			var reads, closed atomic.Int32
			config.Run = func(ctx context.Context, id string) (sourcedelivery.Run, error) {
				reads.Add(1)
				r, err := run(ctx, id)
				if staged.Load() {
					r.Terminal = test.terminal
					if test.changed {
						r.Admission.Digest = strings.Repeat("b", 64)
					}
				}
				return r, err
			}
			config.Stage = func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
				staged.Store(true)
				return sourcedelivery.Staged{Body: &countedStage{strings.NewReader(payload), &closed}, Bytes: int64(len(payload)), Waited: test.waited}, nil
			}
			response := requestRulesSource(t, sourcedelivery.New(config), sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
			if response.Code != test.status || reads.Load() != int32(test.reads) || closed.Load() != 1 {
				t.Fatal("stage wait used stale authorization or lost its reader", response.Code, reads.Load(), closed.Load())
			}
			if test.status == http.StatusForbidden && (strings.Contains(response.Body.String(), payload) || response.Header().Get(sourcedelivery.ReceiptTrailer) != "") {
				t.Fatal("unauthorized staged bytes escaped", response.Body.String())
			}
		})
	}
}
