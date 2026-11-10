package model

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

const DefaultAdmissionHoldDuration = 2 * time.Hour
const MaxAdmissionHoldDuration = 12 * time.Hour

type AdmissionHold struct {
	On        bool      `json:"on"`
	Reason    string    `json:"reason,omitempty"`
	Allow     []string  `json:"allow"`
	Actor     string    `json:"actor,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	ChangedAt time.Time `json:"changed_at,omitzero"`
}

type AdmissionHoldRequest struct {
	On       bool
	Reason   string
	Allow    []string
	Duration time.Duration
}

func (request AdmissionHoldRequest) Normalize() (AdmissionHoldRequest, error) {
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Reason == "" || len(request.Reason) > 512 || strings.IndexFunc(request.Reason, unicode.IsControl) >= 0 {
		return request, fmt.Errorf("reason must contain 1..512 bytes without control characters")
	}
	if !request.On {
		if request.Duration != 0 || request.Allow != nil {
			return request, fmt.Errorf("allow and duration apply only when enabling a hold")
		}
		request.Allow = []string{}
		return request, nil
	}
	if request.Duration == 0 {
		request.Duration = DefaultAdmissionHoldDuration
	}
	if request.Duration < 0 || request.Duration > MaxAdmissionHoldDuration {
		return request, fmt.Errorf("duration must be positive and at most 12h")
	}
	if request.Allow == nil {
		request.Allow = []string{"tag", "release"}
	}
	if len(request.Allow) > 3 {
		return request, fmt.Errorf("allow accepts tag, release and promotion at most once each")
	}
	seen := map[string]bool{}
	for _, class := range request.Allow {
		if (class != "tag" && class != "release" && class != "promotion") || seen[class] {
			return request, fmt.Errorf("allow accepts tag, release and promotion at most once each")
		}
		seen[class] = true
	}
	request.Allow = slices.Clone(request.Allow)
	return request, nil
}
func (hold AdmissionHold) Active(now time.Time) bool { return hold.On && now.Before(hold.ExpiresAt) }
func (hold AdmissionHold) Blocks(run Run, now time.Time) bool {
	if !hold.Active(now) {
		return false
	}
	for _, class := range hold.Allow {
		if class == "tag" && run.RefKind == RefTag || class == "release" && run.Release || class == "promotion" && run.Trigger == "promotion" {
			return false
		}
	}
	return true
}
func (hold AdmissionHold) Snapshot(now time.Time) AdmissionHold {
	hold.On = hold.Active(now)
	hold.Allow = append([]string{}, hold.Allow...)
	return hold
}
func (hold AdmissionHold) Message() string {
	return fmt.Sprintf("admission held by admin: %s, until %s", hold.Reason, hold.ExpiresAt.UTC().Format(time.RFC3339Nano))
}

// ShortMessage is the list-facing hold explanation, the reason line of every
// held run: the admin's whole reason (bounded to 512 bytes when set) and the
// expiry to the second. Message retains detail vocabulary.
func (hold AdmissionHold) ShortMessage() string {
	return fmt.Sprintf("queue held by admin: %s, until %s", hold.Reason, hold.ExpiresAt.UTC().Truncate(time.Second).Format(time.RFC3339))
}
