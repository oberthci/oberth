package gitcache

import "testing"

func TestReceiveCompletionRequiresPreparedPhase(t *testing.T) {
	for _, row := range []struct {
		state          string
		prepared, want bool
	}{
		{"reserved", false, false},
		{"reserved", true, false},
		{"ready", false, false},
		{"ready", true, true},
		{"rejected", false, false},
		{"rejected", true, false},
	} {
		record := Reservation{State: row.state}
		if row.prepared {
			record.Prepared = map[string]struct{}{}
		}
		completion := receiveCompletion{gate: &receiveGate{}}
		if got := completion.accepts(record); got != row.want {
			t.Errorf("completion %s prepared=%t: got %t, want %t", row.state, row.prepared, got, row.want)
		}
	}
}
