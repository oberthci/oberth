package gitssh

import "testing"

// Only a connection that reached no server or lost it is a transport
// failure; an answer (a refused key, identity, session or command), a
// connection ended before Git began, and an unknown line are not.
func TestTransportFailureNamesOnlyTransportReasons(t *testing.T) {
	for _, tc := range []struct {
		line      string
		transport bool
	}{
		{ReasonUnanswered, true},
		{ReasonLost, true},
		{ReasonTransport, true},
		{ReasonEnded, false},
		{ReasonRefused, false},
		{HostKeyMismatchReason, false},
		{"Git SSH identity was refused.", false},
		{"fatal: unexpected answer from the remote", false},
		{"", false},
	} {
		if got := TransportFailure(tc.line + "\nfatal: Could not read from remote repository."); got != tc.transport {
			t.Fatalf("%q: transport %v, want %v", tc.line, got, tc.transport)
		}
	}
}
