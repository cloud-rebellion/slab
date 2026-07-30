package crmsync

import (
	"fmt"
	"testing"
	"time"
)

// The map is keyed on (site, visitor) and every public /t/* request with a
// fresh visitor_id added a permanent entry, so it grew for the process
// lifetime until the container OOMed. This drives enough distinct visitors to
// trigger a sweep and asserts the map does not keep all of them.
func TestThrottlerEvictsStaleEntries(t *testing.T) {
	now := time.Now()
	th := NewThrottler(10 * time.Minute)
	th.clock = func() time.Time { return now }

	const n = sweepEvery * 3
	for i := 0; i < n; i++ {
		th.Allow(Event{SiteID: "s", VisitorID: fmt.Sprintf("v%d", i), Event: "pageview"})
	}
	if got := len(th.lastSeen); got < n/2 {
		t.Fatalf("entries were evicted while still inside the window: %d of %d", got, n)
	}

	// Advance past the window and drive enough calls to hit a sweep.
	now = now.Add(20 * time.Minute)
	for i := 0; i < sweepEvery; i++ {
		th.Allow(Event{SiteID: "s", VisitorID: fmt.Sprintf("fresh%d", i), Event: "pageview"})
	}
	if got := len(th.lastSeen); got > sweepEvery*2 {
		t.Errorf("map holds %d entries after the window elapsed; stale entries are never evicted (unbounded growth)", got)
	}
}
