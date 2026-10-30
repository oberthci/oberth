package service

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const runReadBudget = 10 * time.Second

// Only interactive API/MCP log reads share these slots. Internal CI excerpts
// read the same bounded store directly and must not wait for user scans.
var runLogPatternScans = make(chan struct{}, 2)

func boundedRunReadError(err error) error {
	var refusal *toolRefusal
	if errors.As(err, &refusal) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return refuse(fmt.Errorf("%w: %w", ErrUnavailable, err), "run read budget exhausted; retry a smaller log page or run_get with summary:true")
	}
	return err
}
