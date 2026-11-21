package sourcedelivery_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

// A schedule child's Pod sends the scheduled locator. It names the run whose
// ID is the caller's own Workflow name; every recorded-ownership check still
// applies, and a child the watch has not recorded yet gets a retryable 503.
func TestScheduledLocatorSelectsOnlyTheCallersOwnRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*sourcedelivery.Run) error
		want   int
	}{
		{"own-child", func(*sourcedelivery.Run) error { return nil }, http.StatusOK},
		{"not-recorded-yet", func(*sourcedelivery.Run) error { return errors.New("run not found") }, http.StatusServiceUnavailable},
		{"another-workflow-uid", func(r *sourcedelivery.Run) error { r.UID = "replacement-uid"; return nil }, http.StatusForbidden},
		{"run-id-is-not-the-workflow", func(r *sourcedelivery.Run) error { r.Admission.RunID = "run"; return nil }, http.StatusForbidden},
		{"terminal", func(r *sourcedelivery.Run) error { r.Terminal = true; return nil }, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := deliveryRulesConfig()
			child, err := config.Run(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			child.Admission.RunID = child.Workflow
			changeErr := tc.change(&child)
			var asked []string
			config.Run = func(_ context.Context, id string) (sourcedelivery.Run, error) {
				asked = append(asked, id)
				return child, changeErr
			}
			response := requestLocatedSource(t, sourcedelivery.New(config), sourcecontract.ScheduledLocator)
			if response.Code != tc.want || len(asked) != 1 || asked[0] != "workflow" {
				t.Fatalf("scheduled locator: status %d, runs asked %v", response.Code, asked)
			}
		})
	}
}

func requestLocatedSource(t *testing.T, server *sourcedelivery.Server, locator string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(sourcedelivery.LoadRequest{Locator: locator, Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, sourcedelivery.LoadPath, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer bound-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}
