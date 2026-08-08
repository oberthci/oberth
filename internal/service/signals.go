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
}

func NewSignals() *Signals {
	return &Signals{runs: make(chan struct{}), promotions: make(chan struct{})}
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

func (signals *Signals) NotifyRun(_ string) {
	if signals == nil {
		return
	}
	signals.mu.Lock()
	defer signals.mu.Unlock()
	if signals.runs == nil {
		signals.runs = make(chan struct{})
	}
	close(signals.runs)
	signals.runs = make(chan struct{})
}

func (signals *Signals) NotifyPromotion(_ string) {
	if signals == nil {
		return
	}
	signals.mu.Lock()
	defer signals.mu.Unlock()
	if signals.promotions == nil {
		signals.promotions = make(chan struct{})
	}
	close(signals.promotions)
	signals.promotions = make(chan struct{})
}
