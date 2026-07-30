package crmsync

import (
	"sync"
	"time"
)

// Throttler suppresses repeated visitor events that fire within a short
// window. The goal is to keep BrightCRM's activity log readable: a single
// visitor scrolling through ten pages in a minute should generate one
// activity row, not ten.
//
// Identified events bypass the throttle entirely so we never lose the
// moment a visitor links to a contact.
type Throttler struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	interval time.Duration
	clock    func() time.Time
	// calls counts Allow invocations so eviction runs every sweepEvery calls
	// rather than on a timer. Guarded by mu.
	calls uint64
}

// NewThrottler builds a Throttler. interval is the minimum gap between two
// non-identified events for the same (site_id, visitor_id). Pass 0 to
// disable throttling (every event passes).
func NewThrottler(interval time.Duration) *Throttler {
	return &Throttler{
		lastSeen: make(map[string]time.Time),
		interval: interval,
		clock:    time.Now,
	}
}

// sweepEvery is how many Allow calls pass between eviction sweeps. The map is
// keyed on (site, visitor) and every public /t/* request with a fresh
// visitor_id used to add a permanent entry, so the map grew without bound for
// the process lifetime: roughly 120 bytes each, so 10M requests is over a GB
// of unreclaimable heap and an eventual OOM with no correlated request spike
// (the map rebuilds empty on restart). The per-IP limiter next to it does not
// bound this, because it is keyed on IP and this is keyed on visitor id.
//
// Sweeping inline under the existing mutex is enough; no goroutine needed.
const sweepEvery = 1024

// Allow reports whether event should be forwarded to BrightCRM. Side
// effect: when Allow returns true and the event is throttle-eligible, the
// last-seen timestamp for (site, visitor) is updated.
func (t *Throttler) Allow(event Event) bool {
	if t == nil || t.interval <= 0 {
		return true
	}
	if event.Event == EventIdentified {
		// Identified events always pass and reset the window so an
		// identification doesn't keep a fresh post-identification visit out.
		t.mu.Lock()
		defer t.mu.Unlock()
		t.maybeSweepLocked()
		t.lastSeen[throttleKey(event)] = t.clock()
		return true
	}
	if event.VisitorID == "" {
		// No visitor key to throttle on; let it through and let the CRM
		// decide whether to drop it.
		return true
	}

	key := throttleKey(event)
	now := t.clock()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.maybeSweepLocked()
	last, ok := t.lastSeen[key]
	if ok && now.Sub(last) < t.interval {
		return false
	}
	t.lastSeen[key] = now
	return true
}

// maybeSweepLocked evicts entries older than the throttle window. An entry
// past the window can never cause a throttle decision (Allow would let it
// through anyway), so dropping it changes no behaviour and only reclaims
// memory. Caller must hold t.mu.
func (t *Throttler) maybeSweepLocked() {
	t.calls++
	if t.calls%sweepEvery != 0 {
		return
	}
	cutoff := t.clock().Add(-t.interval)
	for k, seen := range t.lastSeen {
		if seen.Before(cutoff) {
			delete(t.lastSeen, k)
		}
	}
}

// throttleKey is the (site, visitor) tuple used to dedupe events.
func throttleKey(e Event) string {
	return e.SiteID + "|" + e.VisitorID
}
