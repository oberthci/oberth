package api

import (
	"context"
	"path"
	"slices"

	"github.com/oberthci/oberth/internal/model"
)

// EventSubscription narrows the agent events one MCP session receives
// (#1514). The caller's uplink scope always applies first; an empty field
// does not narrow.
type EventSubscription struct {
	Repos    []string `json:"repos,omitempty"`
	RefsGlob string   `json:"refs_glob,omitempty"`
	Kinds    []string `json:"kinds,omitempty"`
	// Own keeps only events about the caller's own runs and promotions.
	Own bool `json:"own,omitempty"`
}

// Matches reports whether event passes the subscription for identity.
func (subscription EventSubscription) Matches(event model.AgentEvent, identity string) bool {
	if len(subscription.Repos) > 0 && !slices.Contains(subscription.Repos, event.Repo) {
		return false
	}
	if len(subscription.Kinds) > 0 && !slices.Contains(subscription.Kinds, event.Kind) {
		return false
	}
	if subscription.RefsGlob != "" {
		if matched, err := path.Match(subscription.RefsGlob, event.Ref); err != nil || !matched {
			return false
		}
	}
	if subscription.Own {
		actor, _ := event.Data["actor"].(string)
		if actor != identity {
			return false
		}
	}
	return true
}

// EventPage is one bounded page of agent events after a cursor.
type EventPage struct {
	Events []model.AgentEvent `json:"events"`
	// NextAfterID is the cursor for the next call: the last event scanned,
	// which may be beyond the last one returned when a filter skipped it.
	NextAfterID int64 `json:"next_after_id"`
	LatestID    int64 `json:"latest_id"`
	OldestID    int64 `json:"oldest_id"`
	HasMore     bool  `json:"has_more"`
	// Gap is true when events after the cursor were already pruned by
	// agentEvents.retention.
	Gap bool `json:"gap,omitempty"`
}

// EventsSubscribeResponse is events_subscribe. Session and Push describe the
// transport the server filled in for this MCP session.
type EventsSubscribeResponse struct {
	Subscription EventSubscription `json:"subscription"`
	LatestID     int64             `json:"latest_id"`
	Session      bool              `json:"session"`
	Push         string            `json:"push"`
}

// EventFeed is the service side of the push stream and resource updates.
type EventFeed interface {
	AgentEventWake() <-chan struct{}
	AgentEventsFor(context.Context, Actor, EventSubscription, int64, int) (EventPage, error)
}

// RunExistence checks that a run is visible to the actor, for
// resources/subscribe and resources/read.
type RunExistence interface {
	Run(context.Context, Actor, string) (any, error)
}
