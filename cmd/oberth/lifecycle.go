package main

import (
	"context"
	"io"
	"sync"
	"time"
)

type stopPhase uint8

const (
	stopWorkers stopPhase = iota
	stopService
	stopResources
)

// Part owns one lifetime. Stop interrupts its listener before Run joins;
// Close releases resources after the join. A stalled part is named and left.
type Part struct {
	Shutdown                   stopPhase
	Name                       string
	Run, Ready, Recover, Close func(context.Context) error
	Stop                       func()
}
type managedPart struct {
	Part
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	ready  bool
}
type lifecycle struct {
	mu                 sync.Mutex
	closing            bool
	parts              []*managedPart
	ctx, phase         context.Context
	cancel, abortStart context.CancelFunc
	failStart          context.CancelCauseFunc
	failures           chan error
	onFailure          func()
	once, startOnce    sync.Once
	stopErr, startErr  error
	timeout            func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	diagnostic         io.Writer
}

func newLifecycle(parent context.Context, parts []Part) *lifecycle {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	phase, fail := context.WithCancelCause(ctx)
	abort := func() { fail(context.Canceled) }
	l := &lifecycle{ctx: ctx, phase: phase, cancel: cancel, abortStart: abort, failStart: fail, failures: make(chan error, 1), timeout: context.WithTimeout, diagnostic: io.Discard}
	for _, p := range parts {
		l.add(p)
	}
	return l
}
func (l *lifecycle) add(p Part) {
	ctx, cancel := context.WithCancel(l.ctx)
	l.parts = append(l.parts, &managedPart{Part: p, ctx: ctx, cancel: cancel})
}
func (l *lifecycle) construct(c *composition, build func(context.Context, *composition) (Part, error)) error {
	ctx, cancel := context.WithCancel(l.ctx)
	unlink := context.AfterFunc(l.phase, cancel)
	p, err := build(ctx, c)
	unlink()
	if err != nil {
		cancel()
		return err
	}
	l.parts = append(l.parts, &managedPart{Part: p, ctx: ctx, cancel: cancel})
	return l.phaseError(l.phase.Err())
}
func (l *lifecycle) start() error {
	l.startOnce.Do(func() { l.startErr = l.startParts() })
	return l.startErr
}
func (l *lifecycle) launch() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		return context.Canceled
	}
	if err := l.phase.Err(); err != nil {
		return l.phaseError(err)
	}
	for _, p := range l.parts {
		if p.Name == "" {
			return missingPart("name")
		}
		if p.Run != nil {
			p.done = make(chan struct{})
			go l.run(p)
		}
	}
	return nil
}

func (l *lifecycle) startParts() error {
	if err := l.launch(); err != nil {
		return err
	}
	for _, phase := range []func(Part) func(context.Context) error{func(p Part) func(context.Context) error { return p.Ready }, func(p Part) func(context.Context) error { return p.Recover }} {
		for _, p := range l.parts {
			if err := l.phase.Err(); err != nil {
				return l.phaseError(err)
			}
			if fn := phase(p.Part); fn != nil {
				if err := fn(l.phase); err != nil {
					return l.phaseError(err)
				}
			}
		}
	}
	l.mu.Lock()
	for _, p := range l.parts {
		p.ready = p.err == nil
	}
	l.mu.Unlock()
	return l.phaseError(l.phase.Err())
}
func (l *lifecycle) run(p *managedPart) {
	defer close(p.done)
	err := p.Run(p.ctx)
	if p.ctx.Err() != nil && (err == nil || cancellationOnly(err)) {
		return
	}
	l.mu.Lock()
	p.err = serverProblem("stopped", "Part: "+p.Name, err)
	p.ready = false
	l.mu.Unlock()
	l.failStart(p.err)
	if l.onFailure != nil {
		l.onFailure()
	}
	select {
	case l.failures <- p.err:
	default:
	}
}
func (l *lifecycle) stop(ctx context.Context) error {
	l.once.Do(func() {
		l.stopListeners()
		defer l.cancel()
		l.stopErr = l.stopOwners(ctx)
	})
	return l.stopErr
}
func (l *lifecycle) stopListeners() {
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		return
	}
	l.closing = true
	l.mu.Unlock()
	l.abortStart()
	for _, p := range l.parts {
		if p.Stop != nil {
			p.cancel()
			p.Stop()
		}
	}
}

func (l *lifecycle) phaseError(err error) error {
	if cancellationOnly(err) {
		if cause := context.Cause(l.phase); cause != nil {
			return cause
		}
	}
	return err
}
