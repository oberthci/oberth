package api_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func TestEventStreamRequestsBatchesOf100(t *testing.T) {
	sizes := []int{100, 100, 50}
	requests := []apicontract.EventsWaitArgs{}
	cursor := int64(0)
	f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
		c.ReplayBacklog = 250
		b.EventsWait.Handler = apicontract.HandlerFunc[apicontract.EventsWaitArgs, apicontract.EventResult](func(_ context.Context, _ apicontract.Caller, args apicontract.EventsWaitArgs) (apicontract.Call[apicontract.EventResult], error) {
			index := len(requests)
			requests = append(requests, args)
			if index >= len(sizes) {
				return nil, fmt.Errorf("unexpected event page %d", index)
			}
			if args.Limit != sizes[index] {
				t.Errorf("page %d requests %d events, want %d", index, args.Limit, sizes[index])
			}
			page := eventBatch(cursor, sizes[index])
			cursor = page.NextAfterID
			return func(context.Context) (apicontract.EventResult, error) { return page, nil }, nil
		})
	})
	initialize(f)
	w := requestEventStream(f)
	if len(requests) != 3 {
		t.Fatal("event batch continuation changed", len(requests), w.Body.String())
	}
	if requests[0].AfterID != nil || requests[1].AfterID == nil || *requests[1].AfterID != 100 || requests[2].AfterID == nil || *requests[2].AfterID != 200 {
		t.Fatal("event batch cursor did not advance", requests)
	}
	if strings.Count(w.Body.String(), `"method":"notifications/events"`) != 3 || strings.Count(w.Body.String(), `"summary":"batch event"`) != 250 || !strings.Contains(w.Body.String(), `"gap":true`) {
		t.Fatal("event batches were lost or exceeded the backlog", w.Body.String())
	}
}

func TestEventStreamAccepts100Refuses101InOneBatch(t *testing.T) {
	for _, count := range []int{100, 101} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			h := &fake.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]{Value: eventBatch(0, count)}
			f := setup(t, func(c *api.Config, b *apicontract.Bindings) { c.ReplayBacklog = 100; b.EventsWait.Handler = h })
			initialize(f)
			w := requestEventStream(f)
			inputs, _, _ := h.Observed()
			if len(inputs) != 1 || inputs[0].Limit != 100 {
				t.Fatal("event batch limit changed", inputs)
			}
			delivered := strings.Contains(w.Body.String(), `"method":"notifications/events"`)
			if delivered != (count == 100) {
				t.Fatal("event batch boundary changed", count, w.Body.String())
			}
		})
	}
}

func eventBatch(after int64, count int) apicontract.EventResult {
	events := make([]model.AgentEvent, count)
	for i := range events {
		events[i] = model.AgentEvent{ID: after + int64(i) + 1, Summary: "batch event"}
	}
	return apicontract.EventResult{EventPage: model.EventPage{Events: events, NextAfterID: after + int64(count), HasMore: true}}
}
func requestEventStream(f *fixture) *streamWriter {
	r := httptest.NewRequest("GET", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("Mcp-Session-Id", "00000000000000000000000000000001")
	w := newStreamWriter()
	f.server.Handler().ServeHTTP(w, r)
	return w
}
