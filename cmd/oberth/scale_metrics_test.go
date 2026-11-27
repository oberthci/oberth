package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestScaleBaselineMetricsEndpoint(t *testing.T) {
	reader, _ := realServerLogReader(t)
	c := testComposition(t)
	t.Cleanup(c.api.BeginDrain)
	c.store = reader.owner.c.store
	if _, err := c.store.Events(t.Context(), stateport.EventFilter{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	c.api.Handler().ServeHTTP(response, req)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "oberth_event_query_seconds_count 1\n") {
		t.Fatal(response.Code, response.Body.String())
	}
}
