package httpserver

import (
	"sync/atomic"
	"time"
)

// throttle limits how often a repeated warning is logged. The messages it
// guards fire on the request path, where an incident would otherwise bury the
// log in thousands of copies of the same line.
type throttle struct {
	// last is the unix second of the most recent allowed call.
	last atomic.Int64
}

// allow reports whether at least d has passed since the last allowed call. The
// compare-and-swap means concurrent requests during an incident produce one
// line between them, not one each.
func (t *throttle) allow(d time.Duration) bool {
	now := time.Now().Unix()
	last := t.last.Load()
	return now-last >= int64(d.Seconds()) && t.last.CompareAndSwap(last, now)
}
