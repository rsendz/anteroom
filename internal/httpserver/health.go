package httpserver

import (
	"sync/atomic"
	"time"

	"github.com/rsendz/anteroom/internal/events"
)

// queueHealth tracks whether the queue store is answering, and decides when an
// outage has lasted long enough to stop holding visitors back.
//
// Failing open is deliberately not the default and deliberately not immediate.
// Releasing everyone the instant a call fails would turn a one-second network
// blip into the stampede anteroom exists to prevent, so the operator has to
// ask for it and then it only happens once the queue has been unreachable
// continuously for the grace period.
type queueHealth struct {
	enabled bool
	grace   time.Duration
	now     func() time.Time

	// unhealthySince is unix milliseconds of the first failure in the current
	// run of failures; 0 means the queue is answering.
	unhealthySince atomic.Int64
	// open records whether we are currently letting visitors past, so the
	// transitions either way are announced exactly once.
	open atomic.Bool
	// repeats rate-limits the "protection is off" message.
	repeats throttle
}

func newQueueHealth(enabled bool, grace time.Duration) *queueHealth {
	return &queueHealth{enabled: enabled, grace: grace, now: time.Now}
}

// observe records the outcome of a queue call.
func (h *queueHealth) observe(failed bool) {
	if !failed {
		h.unhealthySince.Store(0)
		return
	}
	h.unhealthySince.CompareAndSwap(0, h.now().UnixMilli())
}

// unhealthyFor reports how long the queue has been failing, or zero.
func (h *queueHealth) unhealthyFor() time.Duration {
	since := h.unhealthySince.Load()
	if since == 0 {
		return 0
	}
	return h.now().Sub(time.UnixMilli(since))
}

// shouldFailOpen reports whether visitors should now be let through without
// the queue being consulted.
func (h *queueHealth) shouldFailOpen() bool {
	if !h.enabled {
		return false
	}
	elapsed := h.unhealthyFor()
	return elapsed > 0 && elapsed >= h.grace
}

// status is what the dashboard shows during an incident. It is deliberately
// answerable without touching Redis, because Redis being unreachable is
// exactly when someone will be looking at it.
type status struct {
	QueueHealthy    bool   `json:"queue_healthy"`
	FailOpenEnabled bool   `json:"fail_open_enabled"`
	FailingOpen     bool   `json:"failing_open"`
	UnhealthySecs   int64  `json:"unhealthy_secs"`
	FailOpenAfter   int64  `json:"fail_open_after_secs"`
	Message         string `json:"message,omitempty"`
}

func (h *queueHealth) status() status {
	elapsed := h.unhealthyFor()
	s := status{
		QueueHealthy:    elapsed == 0,
		FailOpenEnabled: h.enabled,
		FailingOpen:     h.shouldFailOpen(),
		UnhealthySecs:   int64(elapsed.Seconds()),
		FailOpenAfter:   int64(h.grace.Seconds()),
	}
	switch {
	case s.FailingOpen:
		s.Message = "The queue store is unreachable and anteroom is letting visitors " +
			"straight through. Your origin is unprotected until it recovers."
	case !s.QueueHealthy && h.enabled:
		s.Message = "The queue store is unreachable. Visitors are being held; anteroom " +
			"will start letting them through if this continues."
	case !s.QueueHealthy:
		s.Message = "The queue store is unreachable. Visitors are being held on the waiting page."
	}
	return s
}

// announce logs and emits the transitions into and out of failing open. An
// operator must never find out by accident that their protection is off.
func (s *Server) announceFailOpen(room string, failingOpen bool) {
	if s.health.open.Swap(failingOpen) != failingOpen {
		if failingOpen {
			s.log.Error("anteroom: queue unreachable past the grace period, letting visitors " +
				"through unchecked; the origin is now unprotected")
			s.emitter.Emit(events.New(events.TypeFailingOpen, room, "", map[string]any{
				"unhealthy_secs": int64(s.health.unhealthyFor().Seconds()),
			}))
		} else {
			s.log.Info("anteroom: queue reachable again, holding visitors normally")
			s.emitter.Emit(events.New(events.TypeFailOpenEnded, room, "", nil))
		}
		return
	}
	if !failingOpen {
		return
	}
	// Still failing open: repeat the warning occasionally so it stays visible
	// in a busy log, but not on every request.
	if s.health.repeats.allow(30 * time.Second) {
		s.log.Error("anteroom: still letting visitors through unchecked",
			"unhealthy_for", s.health.unhealthyFor().Round(time.Second))
	}
}
