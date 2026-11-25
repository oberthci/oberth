package sourcedelivery_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestSourceRecordsOnlyAuthenticatedServerFailures(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		phase string
		want  int
	}{
		{"database", publicerr.New(publicerr.Unavailable, "Database unavailable", "Retry later.", nil), "run", 0},
		{"database-integrity", publicerr.New(publicerr.SchemaRefused, "Database corrupt", "Restore a verified backup.", nil), "run", 0},
		{"stage", io.ErrUnexpectedEOF, "stage", 0},
		{"backpressure", &sourcedelivery.Failure{Code: "unavailable", Reason: "export-slot"}, "stage", 0},
		{"stream", io.ErrUnexpectedEOF, "stream", 1},
		{"absent", publicerr.New(publicerr.NotFound, "Run not found", "Select a run.", nil), "run", 0},
		{"forbidden", &sourcedelivery.Failure{Code: "forbidden"}, "stage", 0},
		{"moved", &sourcedelivery.Failure{Code: "gone"}, "stage", 0},
		{"too-large", &sourcedelivery.Failure{Code: "too_large"}, "stage", 0},
		{"authentication", errors.New("review unavailable"), "auth", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := deliveryRulesConfig()
			calls := 0
			c.RecordFailure = func(_ context.Context, who sourcedelivery.Caller, _ time.Time) error {
				calls++
				if who.UID != "workflow-uid" {
					t.Error("unverified identity selected receipt")
				}
				return nil
			}
			switch test.phase {
			case "run":
				c.Run = func(context.Context, string) (sourcedelivery.Run, error) { return sourcedelivery.Run{}, test.cause }
			case "auth":
				c.Authenticate = func(context.Context, string) (sourcedelivery.Caller, error) {
					return sourcedelivery.Caller{}, test.cause
				}
			case "stage":
				c.Stage = func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
					return sourcedelivery.Staged{}, test.cause
				}
			case "stream":
				c.Stage = func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
					return sourcedelivery.Staged{Body: io.NopCloser(brokenSource{})}, nil
				}
			}
			requestRulesSource(t, sourcedelivery.New(c), sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
			if calls != test.want {
				t.Fatal("server failure receipt count", calls, test.want)
			}
		})
	}
}

type brokenSource struct{}

func TestSourceDatabaseFailureAfterDeliveryDeadlineIsTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := deliveryRulesConfig()
		calls := 0
		c.Run = func(ctx context.Context, _ string) (sourcedelivery.Run, error) {
			<-ctx.Done()
			return sourcedelivery.Run{}, ctx.Err()
		}
		c.RecordFailure = func(context.Context, sourcedelivery.Caller, time.Time) error { calls++; return nil }
		reply := requestRulesSource(t, sourcedelivery.New(c), sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
		if reply.Code != 502 || calls != 1 || reply.Header().Get("Retry-After") != "" {
			t.Fatal("exhausted delivery remained retryable", reply.Code, calls)
		}
	})
}

func TestSourceCompletionRecordsSuccessBeforePublishingReceipt(t *testing.T) {
	c := deliveryRulesConfig()
	calls := 0
	c.RecordSuccess = func(context.Context, sourcedelivery.Caller, time.Time) error {
		calls++
		return errors.New("index unavailable")
	}
	reply := requestRulesSource(t, sourcedelivery.New(c), sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
	if calls != 1 || reply.Header().Get(sourcedelivery.ReceiptTrailer) != "" {
		t.Fatal("completion published before durable supersession", calls, reply.Header())
	}
}

func (brokenSource) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
