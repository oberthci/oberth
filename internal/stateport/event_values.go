package stateport

import (
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
	"slices"
)

// EventWrite binds an observation to its durable deduplication key.
type EventWrite struct {
	Event  model.AgentEvent
	Dedupe string
}

// Events uses indexed endpoint seeks and ascending exclusive cursor scans.
type EventFilter struct {
	Repositories   []string
	Upstreams      []string
	After, Through int64
	Limit          int
	Actor          string
}

// Action binds attribution and observation events to one durable fact.
// Events commit with the fact; callers own their kinds and payloads.
type Action struct {
	Actor string
	// Caller is the authenticated decision snapshot for scoped manual actions.
	// Internal fact writers omit it; storage never refreshes authority after a seal.
	Caller actor.Actor
	Reason string
	Events []EventWrite
}

func (a Action) AllowsIssueUpstream(upstream string) bool {
	if a.Caller.Identity == "" {
		return true
	}
	if a.Actor != a.Caller.Identity {
		return false
	}
	if upstream == "" || upstream == "global" {
		return a.Caller.Admin && len(a.Caller.Scope) == 0
	}
	return len(a.Caller.Scope) == 0 || slices.Contains(a.Caller.Scope, upstream)
}

func (a Action) AllowsIssueMove() bool {
	return a.Caller.Identity == "" || a.Actor == a.Caller.Identity && a.Caller.Admin && len(a.Caller.Scope) == 0
}

const (
	RunPageLimit   = 200
	EventPageLimit = 500
)

func (a Action) AllowsIssuePatch(upstream *string) bool {
	return upstream == nil || a.AllowsIssueMove() && a.AllowsIssueUpstream(*upstream)
}

// ScopeKeys shares indexed cursor and retention bounds for repository and upstream events.
// Canonical repositories contain slashes; upstream names cannot, so keys cannot collide.
func (f EventFilter) ScopeKeys() []string {
	if f.Repositories == nil {
		return nil
	}
	return append(slices.Clone(f.Repositories), f.Upstreams...)
}

func EventScope(v model.AgentEvent) string {
	if v.Repo != "" {
		return v.Repo
	}
	if v.Upstream == "global" {
		return ""
	}
	return v.Upstream
}

// ObserveIssue binds caller-owned events to the issue inside its mutation transaction.
func (a Action) ObserveIssue(v model.Issue) {
	for i := range a.Events {
		e := &a.Events[i].Event
		if e.Kind == string(model.EventIssueUpdated) {
			e.Repo = v.Repo
			e.Upstream = v.Upstream
			e.IssueID = v.ID
		}
	}
}
