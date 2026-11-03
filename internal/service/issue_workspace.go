package service

import (
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const (
	DefaultIssueStaleAfter       = 7 * 24 * time.Hour
	DefaultIssueActiveStaleAfter = time.Hour
)

// Overrides belong only to this read. They never update stored issue metadata.
type issueObservationOptions struct {
	StaleAfter       string `json:"stale_after"`
	ActiveStaleAfter string `json:"active_stale_after"`
}

func (service *API) withIssueObservation(options issueObservationOptions) (*API, error) {
	result := *service
	if result.issueStaleAfter == 0 {
		result.issueStaleAfter = DefaultIssueStaleAfter
	}
	if result.issueActiveStaleAfter == 0 {
		result.issueActiveStaleAfter = DefaultIssueActiveStaleAfter
	}
	for _, item := range []struct {
		raw    string
		target *time.Duration
	}{
		{options.StaleAfter, &result.issueStaleAfter},
		{options.ActiveStaleAfter, &result.issueActiveStaleAfter},
	} {
		if item.raw == "" {
			continue
		}
		value, err := time.ParseDuration(item.raw)
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("%w: stale windows must be positive durations", ErrInvalidInput)
		}
		*item.target = value
	}
	// A shorter general override also shortens the inherited active window.
	if options.ActiveStaleAfter == "" {
		result.issueActiveStaleAfter = min(result.issueActiveStaleAfter, result.issueStaleAfter)
	}
	if result.issueActiveStaleAfter > result.issueStaleAfter {
		return nil, fmt.Errorf("%w: active stale window must not exceed the general stale window", ErrInvalidInput)
	}
	result.issueObservedAt = time.Now()
	if result.waitClock != nil {
		result.issueObservedAt = result.waitClock.Now()
	}
	return &result, nil
}

func (service *API) observeIssue(issue model.Issue) model.Issue {
	general, active := service.issueStaleAfter, service.issueActiveStaleAfter
	if general == 0 {
		general = DefaultIssueStaleAfter
	}
	if active == 0 {
		active = min(DefaultIssueActiveStaleAfter, general)
	}
	window := general
	if issue.WorkStatus == model.IssueActive {
		window = active
	}
	now := service.issueObservedAt
	if now.IsZero() {
		now = time.Now()
		if service.waitClock != nil {
			now = service.waitClock.Now()
		}
	}
	issue.Stale = issue.State == model.IssueOpen && now.Sub(issue.UpdatedAt) > window
	return issue
}
