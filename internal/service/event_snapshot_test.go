package service

import (
	"context"

	"github.com/oberthci/oberth/internal/apicontract"
)

// ReadEventSnapshotForTest exercises the paging engine without adding a public tool.
func ReadEventSnapshotForTest(ctx context.Context, s *Service, c apicontract.Caller, q apicontract.EventsWaitArgs) (apicontract.EventResult, error) {
	a, input, err := s.prepareEvents(ctx, c.Actor, eventsInput(q), "events_wait")
	if err != nil {
		return apicontract.EventResult{}, err
	}
	page, err := s.eventsSince(ctx, a, input)
	return apicontract.EventResult{EventPage: page}, err
}
