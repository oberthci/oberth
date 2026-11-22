package gitcache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"k8s.io/utils/clock"
	"sync"
	"sync/atomic"
	"time"
)

type gitBudgets struct{ idle, local, transfer, cleanup, background, refreshWait, headWait time.Duration }

func defaultGitBudgets() gitBudgets {
	return gitBudgets{idle: time.Minute, local: 10 * time.Minute, transfer: time.Hour, cleanup: time.Minute, background: backgroundBound, refreshWait: refreshDeadline, headWait: headDeadline}
}
func transferCommand(args []string) bool {
	switch nativeOperation(args) {
	case "clone", "fetch", "push", "receive-pack", "upload-pack":
		return true
	}
	return false
}
func nativeOperation(args []string) string {
	for len(args) > 0 {
		if args[0] == "-c" && len(args) > 1 {
			args = args[2:]
			continue
		}
		return args[0]
	}
	return ""
}

type commandProgress struct {
	mu    sync.Mutex
	timer interface {
		Stop() bool
		Reset(time.Duration) bool
	}
	now      func() time.Time
	deadline time.Time
	idle     time.Duration
	closed   bool
	paused   int
	cancel   context.CancelCauseFunc
	// moved, when set, records that the forge began sending a pack, not
	// only an error or nothing.
	moved *atomic.Bool
}

func watchProgress(cancel context.CancelCauseFunc, idle time.Duration) *commandProgress {
	p := &commandProgress{idle: idle, cancel: cancel, now: (clock.RealClock{}).Now}
	if idle > 0 {
		p.deadline = p.now().Add(idle)
		p.timer = time.AfterFunc(idle, p.expire)
	}
	return p
}
func (p *commandProgress) touch(n int) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil && !p.closed && p.paused == 0 {
		p.deadline = p.now().Add(p.idle)
		p.timer.Reset(p.idle)
	}
}
func (p *commandProgress) expire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && p.paused == 0 {
		// Stop or Reset may leave a callback queued from an older deadline.
		if remaining := p.deadline.Sub(p.now()); remaining > 0 {
			p.timer.Reset(remaining)
			return
		}
		p.cancel(fmt.Errorf("git idle limit reached: %w", context.DeadlineExceeded))
	}
}
func (p *commandProgress) pause() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused++
	if p.timer != nil {
		p.timer.Stop()
	}
}
func (p *commandProgress) resume() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused--
	if p.timer != nil && !p.closed && p.paused == 0 {
		p.deadline = p.now().Add(p.idle)
		p.timer.Reset(p.idle)
	}
}
func (p *commandProgress) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
	}
}

type commandReader struct {
	io.Reader
	progress *commandProgress
}

func (r commandReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	r.progress.touch(n)
	return n, err
}

type commandWriter struct {
	io.Writer
	cancel   context.CancelCauseFunc
	progress *commandProgress
}

func (w commandWriter) Write(b []byte) (int, error) {
	n, err := w.Writer.Write(b)
	if err != nil && w.cancel != nil {
		w.cancel(err)
	}
	w.progress.touch(n)
	if w.progress.moved != nil && packArriving(b[:n]) {
		w.progress.moved.Store(true)
	}
	return n, err
}

// packArriving reports whether native fetch output shows the forge preparing
// or sending a pack: its relayed object progress or the local receiving
// count. An error the forge prints, relayed or not, is neither.
func packArriving(b []byte) bool {
	for _, phase := range []string{"Enumerating objects", "Counting objects", "Compressing objects", "Receiving objects"} {
		if bytes.Contains(b, []byte(phase)) {
			return true
		}
	}
	return false
}
