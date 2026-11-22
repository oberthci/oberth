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

// The refusal reasons say that the server or the pins refused this
// connection: a refused host key or identity, no usable pin, or a refused
// Git command.
const (
	ReasonIdentityRefused = "Git SSH identity was refused."
	ReasonNoPin           = "No usable Git host key pin."
)

// TransportFailure reports whether native Git diagnostics carry a transport
// reason this command printed.
func TransportFailure(diagnostics string) bool {
	return carries(diagnostics, ReasonUnanswered, ReasonLost, ReasonTransport)
}

// RefusalFailure reports whether native Git diagnostics carry a refusal
// reason this command printed.
func RefusalFailure(diagnostics string) bool {
	return carries(diagnostics, HostKeyMismatchReason, ReasonIdentityRefused, ReasonNoPin, ReasonRefused)
}

func carries(diagnostics string, reasons ...string) bool {
	for _, reason := range reasons {
		if strings.Contains(diagnostics, reason) {
			return true
		}
	}
	return false
}
