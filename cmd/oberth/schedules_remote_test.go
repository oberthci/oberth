package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSchedulesCLIForwardsToTheMCPTools(t *testing.T) {
	const token = "test-schedules-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	for _, test := range []struct {
		arguments []string
		want      string
	}{
		{[]string{"list", "--json"}, `tool=schedule_list args={}`},
		{[]string{"list", "oberth", "--json"}, `tool=schedule_list args={"repo":"oberth"}`},
		{[]string{"pause", "oberth", "nightly", "--reason", "forge saturated", "--json"}, `tool=schedule_pause args={"entry":"nightly","reason":"forge saturated","repo":"oberth"}`},
		{[]string{"resume", "--json", "oberth", "nightly"}, `tool=schedule_resume args={"entry":"nightly","repo":"oberth"}`},
		{[]string{"trigger", "oberth", "nightly", "--force", "--json"}, `tool=schedule_trigger args={"entry":"nightly","force":true,"repo":"oberth"}`},
		{[]string{"trigger", "--force", "oberth", "nightly", "--json"}, `tool=schedule_trigger args={"entry":"nightly","force":true,"repo":"oberth"}`},
	} {
		var output strings.Builder
		if err := runSchedules(context.Background(), test.arguments, &output); err != nil {
			t.Fatalf("%v: %v", test.arguments, err)
		}
		if !strings.Contains(output.String(), test.want) {
			t.Fatalf("%v sent %s, want %s", test.arguments, output.String(), test.want)
		}
	}
	for _, bad := range [][]string{{"pause", "oberth"}, {"trigger"}, {"resume", "a", "b", "c"}, {"list", "a", "b"}} {
		var output strings.Builder
		if err := runSchedules(context.Background(), bad, &output); !errors.Is(err, errUsage) {
			t.Fatalf("schedules %v = %v, want usage error", bad, err)
		}
	}
}
