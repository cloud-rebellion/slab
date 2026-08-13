package ratelimit

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSweeperRunsRegisteredCallbacksOnTick(t *testing.T) {
	var a, b atomic.Int64
	s := New(5 * time.Millisecond)
	s.Register("a", func() { a.Add(1) })
	s.Register("b", func() { b.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.Load() > 0 && b.Load() > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("ticker never fired: a=%d b=%d", a.Load(), b.Load())
}

func TestSweeperStopsOnContextCancel(t *testing.T) {
	var n atomic.Int64
	s := New(2 * time.Millisecond)
	s.Register("n", func() { n.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n.Load() == 0 {
		t.Fatal("ticker never fired before cancel")
	}
	cancel()
	time.Sleep(30 * time.Millisecond)
	settled := n.Load()
	time.Sleep(50 * time.Millisecond)
	if got := n.Load(); got != settled {
		t.Fatalf("sweeper kept running after cancel: %d -> %d", settled, got)
	}
}

// Start must be idempotent: a duplicated wire-up (two callers both
// deciding they own the lifecycle) must not spawn a second goroutine
// hammering the same maps.
func TestSweeperStartIsIdempotent(t *testing.T) {
	var n atomic.Int64
	s := New(time.Hour)
	s.Register("n", func() { n.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	s.Start(ctx)
	s.Start(ctx)

	// With an hour-long tick nothing should fire on its own; the check
	// that matters is that SweepNow still runs each callback exactly
	// once per call regardless of how many Starts happened.
	s.SweepNow()
	if got := n.Load(); got != 1 {
		t.Fatalf("SweepNow ran callback %d times, want 1", got)
	}
}

func TestSweeperNilAndEmptyAreSafe(t *testing.T) {
	var s *Sweeper
	s.Register("x", func() { t.Fatal("nil sweeper must not invoke callbacks") })
	s.SweepNow()
	s.Start(context.Background())
	if s.Len() != 0 || s.Names() != nil {
		t.Fatal("nil sweeper should report empty")
	}

	real := New(0)
	if real.interval != DefaultInterval {
		t.Fatalf("non-positive interval = %v, want fallback %v", real.interval, DefaultInterval)
	}
	real.Register("nil-fn", nil)
	if real.Len() != 0 {
		t.Fatal("nil callback should not be registered")
	}
}

// Registration after Start must still be swept, because Router() builds
// handlers in an order the sweeper does not control.
func TestSweeperPicksUpLateRegistration(t *testing.T) {
	var n atomic.Int64
	s := New(3 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	s.Register("late", func() { n.Add(1) })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n.Load() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("callback registered after Start was never swept")
}
