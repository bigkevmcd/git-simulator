package core

import "time"

// Clock abstracts time so delays and races are deterministic in tests.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RealClock uses the actual wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// ManualClock is an injectable clock for tests. Advance time with Advance().
type ManualClock struct {
	now chan time.Time
}

// NewManualClock creates a ManualClock set to t.
func NewManualClock(t time.Time) *ManualClock {
	mc := &ManualClock{now: make(chan time.Time, 1)}
	mc.Set(t)

	return mc
}

func (m *ManualClock) Now() time.Time {
	select {
	case t := <-m.now:
		m.now <- t
		return t
	default:
		return time.Time{}
	}
}

func (m *ManualClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	go func() {
		deadline := m.Now().Add(d)
		for {
			now := m.Now()
			if !now.Before(deadline) {
				ch <- now
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return ch
}

// Set updates the current time of the ManualClock.
func (m *ManualClock) Set(t time.Time) {
	select {
	case <-m.now:
	default:
	}
	m.now <- t
}

// Advance moves the clock forward by d.
func (m *ManualClock) Advance(d time.Duration) {
	m.Set(m.Now().Add(d))
}
