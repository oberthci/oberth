package service

import "sync"

// Signals provides bounded generation-based notifications. One run channel
// and one promotion channel may wake unrelated waiters, which re-read durable
// state before waiting again; closing and replacing a channel cannot leak an
// entry per completed object or lose a wakeup between the read and wait.
type Signals struct {
	mu         sync.Mutex
	runs       chan struct{}
	promotions chan struct{}
	// observer receives every notified ID (#1514). It must not block.
	observer func(kind, id string)
}

// Observe installs the change observer the agent event recorder uses. The
// observer runs outside the lock and must not block.
func (signals *Signals) Observe(observer func(kind, id string)) {
	if signals == nil {
		return
	}
	signals.mu.Lock()
	defer signals.mu.Unlock()
	signals.observer = observer
}

func (signals *Signals) observe(kind, id string) {
	signals.mu.Lock()
	observer := signals.observer
	signals.mu.Unlock()
	if observer != nil && id != "" {
		observer(kind, id)
	}
}

func NewSignals() *Signals {
	return &Signals{
		runs: make(chan struct{}), promotions: make(chan struct{}),
	}
}

func (signals *Signals) Run(_ string) <-chan struct{} {
	if signals == nil {
		return nil
	}
	signals.mu.Lock()
	defer signals.mu.Unlock()
	if signals.runs == nil {
		signals.runs = make(chan struct{})
	}
	return signals.runs
}

func (signals *Signals) Promotion(_ string) <-chan struct{} {
	if signals == nil {
		return nil
	}
	signals.mu.Lock()
	defer signals.mu.Unlock()
	if signals.promotions == nil {
		signals.promotions = make(chan struct{})
	}
	return signals.promotions
}

func (signals *Signals) NotifyRun(id string) {
	if signals == nil {
		return
	}
	signals.mu.Lock()
	if signals.runs == nil {
		signals.runs = make(chan struct{})
	}
	close(signals.runs)
	signals.runs = make(chan struct{})
	signals.mu.Unlock()
	signals.observe("run", id)
}

func (signals *Signals) NotifyPromotion(id string) {
	if signals == nil {
		return
	}
	signals.mu.Lock()
	if signals.promotions == nil {
		signals.promotions = make(chan struct{})
	}
	close(signals.promotions)
	signals.promotions = make(chan struct{})
	signals.mu.Unlock()
	signals.observe("promotion", id)
}
