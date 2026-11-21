package publicerr

import (
	"encoding/json"
	"strings"
)

// Code is the complete public refusal taxonomy. Its zero value is invalid.
type Code string

const (
	NotFound          Code = "not_found"
	InvalidInput      Code = "invalid_input"
	InvalidTransition Code = "invalid_transition"
	Ambiguous         Code = "ambiguous"
	Conflict          Code = "conflict"
	Forbidden         Code = "forbidden"
	Unavailable       Code = "unavailable"
	SchemaRefused     Code = "schema_refused"
	Invariant         Code = "invariant"
)

func (c Code) Valid() bool {
	switch c {
	case NotFound, InvalidInput, InvalidTransition, Ambiguous, Conflict, Forbidden, Unavailable, SchemaRefused, Invariant:
		return true
	}
	return false
}

// Error separates deliberately public diagnostics from its private cause.
// Reasons fit 80 characters and remedies fit 120, one sentence each. Detail
// carries optional public diagnostics; Context carries required public information.
// Construct with New; the private cause is never rendered.
type Error struct {
	// ID identifies an optional refusal subtype independently of its wording.
	ID      string
	Context []string
	Detail  string
	code    Code
	details []message
	cause   error
}

// New constructs a public refusal. An invalid code or missing, overlong or
// incomplete diagnostics are programming errors and panic.
func New(code Code, reason, remedy string, cause error) *Error {
	if !code.Valid() || strings.TrimSpace(reason) == "" || strings.TrimSpace(remedy) == "" || CheckMessage(reason, 80) != nil || CheckMessage(remedy, 120) != nil {
		panic("publicerr.New requires a valid code and complete bounded reason/remedy")
	}
	return &Error{code: code, details: []message{{reason, remedy}}, cause: cause}
}
func (e *Error) Code() Code {
	if e == nil || !e.code.Valid() {
		return Invariant
	}
	return e.code
}
func (e *Error) Reason() string {
	items := e.messages()
	if len(items) > 1 {
		return "Several problems blocked this operation."
	}
	return items[0].reason
}
func (e *Error) Remedy() string {
	items := e.messages()
	if len(items) > 1 {
		return "Review the detailed problems and retry."
	}
	return items[0].remedy
}
func (e *Error) messageText() string {
	var pairs []string
	for _, item := range e.messages() {
		pairs = append(pairs, item.reason+" - "+item.remedy)
	}
	return strings.Join(pairs, "; ")
}
func (e *Error) detailText() string {
	var parts []string
	if len(e.messages()) > 1 {
		parts = append(parts, e.messageText())
	}
	if e != nil && e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	return strings.Join(parts, "\n")
}
func (e *Error) Error() string {
	text := string(e.Code()) + ": " + e.messageText()
	if e != nil && e.Detail != "" {
		text += "\n" + e.Detail
	}
	return text
}

type message struct{ reason, remedy string }

func (e *Error) messages() []message {
	if e == nil || len(e.details) == 0 {
		return []message{{"Operation failed.", "Check the request and current state, then retry."}}
	}
	return e.details
}
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID      string   `json:"id,omitempty"`
		Context []string `json:"context,omitempty"`
		Code    Code     `json:"code"`
		Reason  string   `json:"reason"`
		Remedy  string   `json:"remedy"`
		Detail  string   `json:"detail,omitempty"`
	}{e.ID, e.Context, e.Code(), e.Reason(), e.Remedy(), e.detailText()})
}
func unknown(cause error) *Error {
	return &Error{code: Invariant, cause: cause}
}
