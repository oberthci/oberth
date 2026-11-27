package sourcedelivery

import (
	"context"
	"errors"
	"fmt"
	"k8s.io/utils/clock"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Phase labels and summary storage are fixed. Durations can overlap; they must
// never be added to infer caller latency. Total is measured independently.
type Phase uint8

const (
	Auth Phase = iota
	Export
	Archive
	Transfer
	Repository
	Recovery
	ReceiveSlot
	ReceiveGate
	Head
	Object
	Refresh
	PhaseCount
)

func (p Phase) String() string {
	if p >= PhaseCount {
		return "other"
	}
	return [...]string{"auth", "export", "archive", "transfer", "repository", "recovery", "receive-slot", "receive-gate", "head", "object", "refresh"}[p]
}

const CapacityReason = "Source capacity busy"
const CapacityRefusal = "Source delivery capacity is full."
const CapacityRemedy = "Inspect source capacity and native workflow load before trying again."

// Summary contains no caller-controlled strings. Metric observers run once at
// completion and must only update counters and attempt a nonblocking enqueue.
type Summary struct {
	Reason             string
	Kind, Code         string
	Phase              Phase
	Blocked, Cancelled bool
	Total              time.Duration
	Wait, Service      [PhaseCount]time.Duration
	Bytes              int64
	Leaders, Joiners   uint64
}

// Permits exposes current and peak occupancy without allocating labels or
// emitting phase events. Owners enter only after acquiring their real permit.
type Permits struct {
	Active, Peak [PhaseCount]atomic.Int64
}

func (p *Permits) Enter(phase Phase) func() {
	if p == nil {
		return func() {}
	}
	n := p.Active[phase].Add(1)
	for old := p.Peak[phase].Load(); n > old; old = p.Peak[phase].Load() {
		if p.Peak[phase].CompareAndSwap(old, n) {
			break
		}
	}
	return func() { p.Active[phase].Add(-1) }
}

func (s Summary) Detail() string {
	detail := fmt.Sprintf("%s %s reason=%s phase=%s blocked=%t cancelled=%t total=%s bytes=%d leaders=%d joiners=%d", s.Kind, s.Code, s.Reason, s.Phase, s.Blocked, s.Cancelled, s.Total, s.Bytes, s.Leaders, s.Joiners)
	for p := range PhaseCount {
		if s.Wait[p] != 0 || s.Service[p] != 0 {
			detail += fmt.Sprintf(" %s_wait=%s %s_service=%s", p, s.Wait[p], p, s.Service[p])
		}
	}
	return detail
}

type observationContext struct{}
type Observation struct {
	mu      sync.Mutex
	start   time.Time
	summary Summary
}

func Observe(ctx context.Context, kind string) (context.Context, *Observation) {
	o := newObservation(kind)
	return context.WithValue(ctx, observationContext{}, o), o
}

func newObservation(kind string) *Observation {
	return &Observation{start: (clock.RealClock{}).Now(), summary: Summary{Kind: kind}}
}

// Measure records service or waiting in the existing request/job owner.
func Measure(ctx context.Context, phase Phase, waiting bool) func() {
	o, _ := ctx.Value(observationContext{}).(*Observation)
	if o == nil {
		return func() {}
	}
	start := (clock.RealClock{}).Now()
	if waiting {
		o.Block(phase)
	}
	return func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		if waiting {
			o.summary.Wait[phase] += (clock.RealClock{}).Since(start)
		} else {
			o.summary.Service[phase] += (clock.RealClock{}).Since(start)
		}
	}
}

func (o *Observation) Block(phase Phase) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.summary.Phase, o.summary.Blocked = phase, true
}

func Joined(ctx context.Context) {
	if o, _ := ctx.Value(observationContext{}).(*Observation); o != nil {
		o.mu.Lock()
		o.summary.Joiners++
		o.mu.Unlock()
	}
}

// InheritWait carries a shared builder's blocking phase into this load's
// diagnostic. Physical service durations remain owned and counted by the job.
func InheritWait(ctx context.Context, job *Observation) {
	if job == nil {
		return
	}
	s := job.Finish("", nil)
	if o, _ := ctx.Value(observationContext{}).(*Observation); o != nil && s.Blocked {
		o.Block(s.Phase)
	}
}

func (o *Observation) Finish(code string, err error) Summary {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.summary
	s.Code, s.Total = code, (clock.RealClock{}).Since(o.start)
	s.Cancelled = errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	return s
}

// outcomeWriter keeps the final handler outcome even when streaming has already
// sent headers. Unwrap preserves native read/write deadlines.
type outcomeWriter struct {
	reason string
	err    error
	http.ResponseWriter
	code        string
	observation *Observation
	bytes       int64
}

func (w *outcomeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *outcomeWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if w.Header().Get("Content-Type") == LoadContentType {
		w.bytes += int64(n)
	}
	return n, err
}
func outcome(w http.ResponseWriter, code string) {
	if w, ok := w.(*outcomeWriter); ok {
		w.code = code
		if code == "unavailable" {
			w.reason = "other"
		}
	}
}

func (w *outcomeWriter) block(phase Phase) {
	w.observation.mu.Lock()
	defer w.observation.mu.Unlock()
	if !w.observation.summary.Blocked {
		w.observation.summary.Phase, w.observation.summary.Blocked = phase, true
	}
}

func outcomeError(w http.ResponseWriter, err error) {
	if w, ok := w.(*outcomeWriter); ok {
		w.err = err
	}
}
