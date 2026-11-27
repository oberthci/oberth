package sourcedelivery_test

import (
	"context"
	"k8s.io/utils/clock"
	"net/http"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestEveryLoadHasOneFinalSummaryIncludingSuccessAndCapacity(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		phase  sourcedelivery.Phase
	}{
		{"ok", 200, sourcedelivery.Auth},
		{"forbidden", 403, sourcedelivery.Auth},
		{"unavailable", 503, sourcedelivery.Export},
		{"unavailable", 503, sourcedelivery.Archive},
	} {
		t.Run(tc.code+tc.phase.String(), func(t *testing.T) {
			cfg := deliveryRulesConfig()
			var summaries []sourcedelivery.Summary
			cfg.Metric = func(s sourcedelivery.Summary) { summaries = append(summaries, s) }
			if tc.code == "forbidden" {
				cfg.Authenticate = func(context.Context, string) (sourcedelivery.Caller, error) {
					return sourcedelivery.Caller{}, &sourcedelivery.Failure{Code: tc.code}
				}
			}
			if tc.status == 503 {
				cfg.Stage = func(ctx context.Context, _ sourcecontract.Binding) (sourcedelivery.Staged, error) {
					defer sourcedelivery.Measure(ctx, tc.phase, true)()
					return sourcedelivery.Staged{}, &sourcedelivery.Failure{Code: "unavailable", Reason: "export-slot"}
				}
			}
			response := requestRulesSource(t, sourcedelivery.New(cfg), sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
			if response.Code != tc.status || len(summaries) != 1 || summaries[0].Code != tc.code {
				t.Fatalf("response %d summaries %+v", response.Code, summaries)
			}
			if tc.status == http.StatusServiceUnavailable && (response.Body.String() != "{\"version\":1,\"code\":\"unavailable\"}\n" || response.Header().Get("Retry-After") != "1") {
				t.Fatal("wire changed", response)
			}
		})
	}
}

func TestMeasuredDelayUsesSeparateWaitAndService(t *testing.T) {
	ctx, o := sourcedelivery.Observe(t.Context(), "load")
	start := (clock.RealClock{}).Now()
	end := sourcedelivery.Measure(ctx, sourcedelivery.Archive, true)
	(clock.RealClock{}).Sleep(20 * time.Millisecond)
	end()
	actual := (clock.RealClock{}).Since(start)
	s := o.Finish("unavailable", context.DeadlineExceeded)
	if !s.Blocked || !s.Cancelled || s.Phase != sourcedelivery.Archive || s.Service[sourcedelivery.Archive] != 0 || s.Wait[sourcedelivery.Archive] < actual*95/100 || s.Wait[sourcedelivery.Archive] > actual*105/100 {
		t.Fatalf("lost measured wait: %+v actual=%s", s, actual)
	}
}
