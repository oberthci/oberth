package installer

import (
	"context"
	"fmt"
	"io"
	"time"
)

func measureNetworkPolicy(ctx context.Context, cfg Config, deps Deps) NetworkPolicyProbeResult {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	return measureNetworkPolicyProgress(ctx, cfg, deps, ticker.C)
}

func measureNetworkPolicyProgress(ctx context.Context, cfg Config, deps Deps, ticks <-chan time.Time) NetworkPolicyProbeResult {
	output := deps.Output
	if output == nil {
		output = io.Discard
	}
	_, _ = fmt.Fprintln(output, "Checking NetworkPolicy enforcement with a scratch namespace and probe Pods...")
	probe := deps.ProbeNetworkPolicy
	if probe == nil {
		probe = ProbeNetworkPolicy
	}
	result := make(chan NetworkPolicyProbeResult, 1)
	go func() { result <- probe(ctx, cfg, deps) }()
	// Wait for the probe's owned cleanup, including after cancellation. Only
	// this goroutine writes progress, and no heartbeat survives the result.
	for {
		select {
		case value := <-result:
			return value
		case <-ticks:
			_, _ = fmt.Fprintln(output, "Still checking NetworkPolicy enforcement and cleanup...")
		}
	}
}
