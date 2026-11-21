package fake

import (
	"fmt"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/api"
)

type Clock struct {
	mu      sync.Mutex
	now     time.Time
	next    int
	ticks   map[chan time.Time]tick
	Started chan time.Duration
}

func NewClock() *Clock {
	return &Clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ticks: map[chan time.Time]tick{}, Started: make(chan time.Duration, 128)}
}
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for ch, tick := range c.ticks {
		if c.now.Before(tick.next) {
			continue
		}
		tick.next = c.now.Add(tick.interval)
		c.ticks[ch] = tick
		select {
		case ch <- c.now:
		default:
		}
	}
}
func (c *Clock) Ticker(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.ticks[ch] = tick{d, c.now.Add(d)}
	select {
	case c.Started <- d:
	default:
	}
	return ch, func() { c.mu.Lock(); delete(c.ticks, ch); c.mu.Unlock() }
}
func (c *Clock) Nonce() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	return fmt.Sprintf("%032x", c.next)
}
func (c *Clock) Runtime() api.Runtime {
	return api.Runtime{Now: c.Now, Ticker: c.Ticker, Nonce: c.Nonce}
}
func (c *Clock) ActiveTickers() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.ticks) }

type tick struct {
	interval time.Duration
	next     time.Time
}
