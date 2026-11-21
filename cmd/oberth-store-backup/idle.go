package main

import (
	"context"
	"time"
)

const idleLimit = 5 * time.Minute

type idleTimer func(time.Duration, func()) (reset func(), stop func())

func startIdleTimer(limit time.Duration, expire func()) (func(), func()) {
	timer := time.AfterFunc(limit, expire)
	return func() { timer.Reset(limit) }, func() { timer.Stop() }
}

// Each positive stream transfer resets the idle limit; total duration is unbounded.
func idleContext(parent context.Context, limit time.Duration, start idleTimer) (context.Context, func(), func()) {
	ctx, cancel := context.WithCancelCause(parent)
	reset, stop := start(limit, func() { cancel(recoveryError(idleStalled, nil)) })
	return ctx, reset, func() { stop(); cancel(nil) }
}
