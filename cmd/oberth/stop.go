package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (l *lifecycle) stopOwners(ctx context.Context) error {
	var result error
	for phase := stopWorkers; phase <= stopResources; phase++ {
		for i := len(l.parts) - 1; i >= 0; i-- {
			p := l.parts[i]
			if p.Shutdown != phase {
				continue
			}
			budget := 5 * time.Second
			if phase == stopService {
				budget = 10 * time.Second
			}
			stop, cancel := l.timeout(ctx, budget)
			joined, err := l.stopPart(stop, p)
			cancel()
			result = errors.Join(result, err)
			if err != nil {
				_, _ = fmt.Fprintln(l.diagnostic, shutdownNotice(p.Name, err))
			}
			if !joined {
				return result
			}
		}
	}
	return result
}

func (l *lifecycle) stopPart(ctx context.Context, p *managedPart) (bool, error) {
	p.cancel()
	if p.done != nil {
		select {
		case <-p.done:
		case <-ctx.Done():
			return false, serverProblem("shutdown", "Part: "+p.Name, ctx.Err())
		}
	}
	if p.Close == nil {
		return true, p.err
	}
	// A deadline refusal leaves the owner's dependencies open.
	done := make(chan error, 1)
	go func() { done <- p.Close(ctx) }()
	select {
	case err := <-done:
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded), errors.Join(p.err, err)
	case <-ctx.Done():
		return false, errors.Join(p.err, serverProblem("shutdown", "Part: "+p.Name, ctx.Err()))
	}
}

func shutdownNotice(name string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "part " + name + " did not stop within its shutdown share"
	}
	return "part " + name + " failed during shutdown"
}
