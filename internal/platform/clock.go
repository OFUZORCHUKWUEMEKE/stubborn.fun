package platform

import (
	"sync"
	"time"
)

// Clock is the time source for domain logic. Domain packages take a Clock
// rather than calling time.Now() directly so schedulers, close-at
// deadlines, and dispute windows are deterministically testable. It lives
// in platform because both market (auto-close) and settle (dispute
// window) need it — and, more importantly, both need the same FakeClock
// rather than two subtly different ones.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real wall-clock implementation used in production.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FakeClock is a manually advanced Clock for tests. It is safe for
// concurrent use: schedulers read Now() from their own goroutine while a
// test advances the clock from another.
type FakeClock struct {
	mu  sync.RWMutex
	now time.Time
}

func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t.UTC()}
}

func (c *FakeClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to an absolute time.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
}
