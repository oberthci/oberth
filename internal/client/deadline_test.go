package client

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNonPollUsesDeclaredDeadline(t *testing.T) {
	d := definition(t, "repo_list")
	d.Deadline = 7 * time.Minute
	if got := callTimeout(d, json.RawMessage(`{}`)); got != 7*time.Minute+30*time.Second {
		t.Fatalf("declared deadline ignored: %v", got)
	}
}
