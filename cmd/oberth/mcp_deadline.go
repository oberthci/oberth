package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/service"
)

const mcpDeadlineMargin = 30 * time.Second

// mcpDeadline gives long-poll tools room to return their terminal response.
// Omitted/zero wait timeouts use the server's default ceiling, not 60 seconds.
func mcpDeadline(cfg mcpFlagResult, params any) (time.Duration, time.Duration, error) {
	var toolTimeout time.Duration
	if call, ok := params.(toolsCallParams); ok {
		if call.Name == "wait" || call.Name == "promote_status" {
			toolTimeout = service.DefaultMaximumWait
		}
		var arguments map[string]json.RawMessage
		if json.Unmarshal(call.Arguments, &arguments) == nil {
			if raw, exists := arguments["timeout"]; exists {
				var seconds int64
				if json.Unmarshal(raw, &seconds) == nil && seconds > 0 {
					const maximumSeconds = int64((time.Duration(1<<63-1) - mcpDeadlineMargin) / time.Second)
					if seconds > maximumSeconds {
						return 0, 0, fmt.Errorf("%w: tool timeout exceeds the client duration limit", errUsage)
					}
					toolTimeout = time.Duration(seconds) * time.Second
				}
			}
		}
	}
	if cfg.timeout > 0 {
		return cfg.timeout, toolTimeout, nil
	}
	limit := mcpRequestTimeout
	if toolTimeout+mcpDeadlineMargin > limit {
		limit = toolTimeout + mcpDeadlineMargin
	}
	return limit, toolTimeout, nil
}

func mcpDeadlineError(limit, toolTimeout time.Duration, err error) error {
	if toolTimeout > 0 {
		return fmt.Errorf("client deadline %s exceeded (tool timeout %s): %w", limit, toolTimeout, err)
	}
	return fmt.Errorf("client deadline %s exceeded: %w", limit, err)
}
