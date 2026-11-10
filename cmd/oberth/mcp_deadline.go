package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/oberthci/oberth/internal/api"
)

const mcpDeadlineMargin = 30 * time.Second

// mcpDeadline gives blocking tools room to return their terminal response.
// The bound comes from the tool's own timeout argument, whatever its spelling
// (timeout, timeout_s, timeout_seconds), and when that is omitted from the
// bound the tool's published schema says the server applies. Tools without a
// timeout argument keep the 60-second default; --timeout overrides both.
func mcpDeadline(cfg mcpFlagResult, params any) (time.Duration, time.Duration, error) {
	var toolTimeout time.Duration
	if call, ok := params.(toolsCallParams); ok {
		toolTimeout = api.ToolTimeouts()[call.Name].Default
		var arguments map[string]json.RawMessage
		if json.Unmarshal(call.Arguments, &arguments) == nil {
			for _, name := range mcpTimeoutArguments(call.Name) {
				raw, exists := arguments[name]
				if !exists {
					continue
				}
				var seconds int64
				if json.Unmarshal(raw, &seconds) == nil && seconds > 0 {
					const maximumSeconds = int64((time.Duration(1<<63-1) - mcpDeadlineMargin) / time.Second)
					if seconds > maximumSeconds {
						return 0, 0, fmt.Errorf("%w: tool timeout exceeds the client duration limit", errUsage)
					}
					toolTimeout = time.Duration(seconds) * time.Second
				}
				break
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

// mcpTimeoutArguments lists the argument names that may carry a tool's own
// bound: the schema's spelling first for known tools, every spelling for
// tools this client does not know (a newer server).
func mcpTimeoutArguments(tool string) []string {
	if known, ok := api.ToolTimeouts()[tool]; ok {
		return []string{known.Argument}
	}
	return []string{"timeout", "timeout_s", "timeout_seconds"}
}

func mcpDeadlineError(limit, toolTimeout time.Duration, err error) error {
	// A timed-out body can end with EOF or a parser error instead of returning
	// the context error. Keep both the known deadline cause and that detail.
	if !errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
	}
	if toolTimeout > 0 {
		return fmt.Errorf("client deadline %s exceeded (tool timeout %s): %w", limit, toolTimeout, err)
	}
	return fmt.Errorf("client deadline %s exceeded: %w", limit, err)
}

// mcpRetryParams shrinks a reconnecting long poll's server-side timeout to
// fit the remaining client budget, keeping a margin for the reply. Arguments
// that already fit are sent unchanged.
func mcpRetryParams(params any, toolTimeout, remaining time.Duration) any {
	call, ok := params.(toolsCallParams)
	if !ok || toolTimeout <= 0 {
		return params
	}
	known, ok := api.ToolTimeouts()[call.Name]
	if !ok {
		return params
	}
	seconds := int64((remaining - min(mcpDeadlineMargin, remaining/2)) / time.Second)
	if seconds >= int64(toolTimeout/time.Second) {
		return params
	}
	var arguments map[string]json.RawMessage
	if len(call.Arguments) > 0 && json.Unmarshal(call.Arguments, &arguments) != nil {
		return params
	}
	if arguments == nil {
		arguments = map[string]json.RawMessage{}
	}
	arguments[known.Argument] = json.RawMessage(strconv.FormatInt(max(seconds, 1), 10))
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return params
	}
	call.Arguments = encoded
	return call
}
