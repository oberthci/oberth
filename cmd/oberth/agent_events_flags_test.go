package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Agent event and MCP push bounds have binary defaults and are validated.
func TestServeAgentEventFlagsBounded(t *testing.T) {
	defaults, err := parseServeOptions([]string{"--argo-namespace=argo"}, io.Discard)
	if err != nil || defaults.agentEvents.Retention != 10000 || defaults.agentEvents.SyncRetention != 20000 || defaults.agentEvents.Interval != 10*time.Second ||
		defaults.agentEvents.LockWait != time.Minute || defaults.agentEventStreams != 4 || defaults.agentEventBacklog != 500 || defaults.mcpHeartbeat != 20*time.Second {
		t.Fatalf("defaults = %+v streams=%d backlog=%d heartbeat=%s, %v", defaults.agentEvents, defaults.agentEventStreams, defaults.agentEventBacklog, defaults.mcpHeartbeat, err)
	}
	configured, err := parseServeOptions([]string{"--argo-namespace=argo", "--agent-events-retention=50", "--agent-events-sync-retention=60", "--agent-events-interval=30s",
		"--agent-events-lock-wait-threshold=2m", "--agent-events-max-streams-per-token=2", "--agent-events-max-backlog=10", "--agent-events-heartbeat=5s"}, io.Discard)
	if err != nil || configured.agentEvents.Retention != 50 || configured.agentEvents.Interval != 30*time.Second || configured.agentEventStreams != 2 || configured.mcpHeartbeat != 5*time.Second {
		t.Fatalf("configured = %+v, %v", configured.agentEvents, err)
	}
	for _, value := range []string{"--agent-events-retention=0", "--agent-events-sync-retention=2000000", "--agent-events-interval=1s", "--agent-events-interval=10m",
		"--agent-events-lock-wait-threshold=0s", "--agent-events-max-streams-per-token=65", "--agent-events-max-backlog=0", "--agent-events-heartbeat=10m"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", value}, io.Discard); err == nil {
			t.Fatal("out-of-range agent event flag accepted", value)
		}
	}
}

func TestChartRendersAgentEventFlagsWithBinaryDefaults(t *testing.T) {
	render := func(sets ...string) (string, error) {
		args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + goProxyDigest}
		for _, set := range sets {
			args = append(args, "--set", set)
		}
		output, err := exec.Command("helm", args...).CombinedOutput()
		return string(output), err
	}
	text, err := render()
	if err != nil {
		t.Fatalf("default chart: %v %s", err, text)
	}
	for _, want := range []string{"- --agent-events-retention=10000", "- --agent-events-sync-retention=20000", `"--agent-events-interval=10s"`, `"--agent-events-lock-wait-threshold=60s"`,
		"- --agent-events-max-streams-per-token=4", "- --agent-events-max-backlog=500", `"--agent-events-heartbeat=20s"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("default chart misses %q", want)
		}
	}
	text, err = render("agentEvents.maxStreamsPerToken=8", "agentEvents.interval=30s")
	if err != nil || !strings.Contains(text, "- --agent-events-max-streams-per-token=8") || !strings.Contains(text, `"--agent-events-interval=30s"`) {
		t.Fatalf("agentEvents values not rendered: %v", err)
	}
	for _, set := range []string{"agentEvents.maxStreamsPerToken=0", "agentEvents.maxBacklog=20000", "agentEvents.interval=soon", "agentEvents.bogus=1"} {
		if _, err := render(set); err == nil {
			t.Fatal("chart schema accepted", set)
		}
	}
}
