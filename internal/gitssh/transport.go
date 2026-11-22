package gitssh

import "strings"

// The transport reasons say that the connection reached no server or lost it.
// A caller may read them as a slow or unreachable forge, unlike a refused
// host key or identity, a session or command the server refused
// (ReasonRefused), or a connection the server ended before Git began
// (ReasonEnded, which an identity refusal can look like).
const (
	ReasonUnanswered = "The Git server did not answer."
	ReasonLost       = "The Git server went away during the transfer."
	ReasonTransport  = "Git SSH transport is unavailable."
	ReasonEnded      = "The Git server ended the connection before Git began."
	ReasonRefused    = "The Git server refused the Git command."
)

// TransportFailure reports whether native Git diagnostics carry a transport
// reason this command printed.
func TransportFailure(diagnostics string) bool {
	for _, reason := range []string{ReasonUnanswered, ReasonLost, ReasonTransport} {
		if strings.Contains(diagnostics, reason) {
			return true
		}
	}
	return false
}
