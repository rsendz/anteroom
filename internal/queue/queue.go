// Package queue holds the waiting-room state: who is waiting, in what order,
// who is currently admitted, and how fast admissions may happen. Redis is the
// source of truth so that several anteroom replicas share one fair queue.
package queue

import (
	"context"
	"time"
)

// RoomConfig is the runtime-adjustable half of a room's configuration. It
// lives in Redis rather than the config file so that changes made through the
// admin API take effect immediately and survive a restart.
type RoomConfig struct {
	Rate         float64
	MaxActive    int
	SessionTTL   time.Duration
	AbandonAfter time.Duration
	Paused       bool
	// JoinLimit caps how many visitors may newly enter the queue from one
	// address per JoinWindow. Zero disables the limit.
	JoinLimit  int
	JoinWindow time.Duration

	// Lottery settles the order of everyone collected before the doors open by
	// drawing rather than by arrival, so turning up early gains nothing.
	Lottery bool
	// DrawSalt keeps draw places from being worked out before the room exists.
	// It is generated once per room and never leaves Redis.
	DrawSalt string

	// The schedule. A zero time means unset.
	QueueOpensAt time.Time
	AdmitsAt     time.Time
	ClosesAt     time.Time
}

// Snapshot is a point-in-time view of one room, used by the admin API and the
// dashboard.
type Snapshot struct {
	Room      string  `json:"room"`
	Waiting   int64   `json:"waiting"`
	Active    int64   `json:"active"`
	Rate      float64 `json:"rate"`
	MaxActive int     `json:"max_active"`
	// Seconds, so the JSON stays readable in the dashboard.
	SessionTTLSecs   int64 `json:"session_ttl_secs"`
	AbandonAfterSecs int64 `json:"abandon_after_secs"`
	Paused           bool  `json:"paused"`
	JoinLimit        int   `json:"join_limit"`
	JoinWindowSecs   int64 `json:"join_window_secs"`
	TotalJoined      int64 `json:"total_joined"`
	TotalAdmitted    int64 `json:"total_admitted"`
	TotalExpired     int64 `json:"total_expired"`
	TotalAbandoned   int64 `json:"total_abandoned"`
	// TotalRefused counts visitors turned away by the per-address join limit.
	// A climbing number means the limit is too tight for the traffic, so it is
	// surfaced rather than hidden.
	TotalRefused int64 `json:"total_refused"`

	// Phase and the schedule behind it, for the countdown on the waiting page
	// and the timetable on the dashboard.
	Phase   Phase `json:"phase"`
	Lottery bool  `json:"lottery"`
	// Unix milliseconds, 0 when unset, so the browser can count down without
	// depending on the visitor's own clock being right.
	QueueOpensAtMS int64 `json:"queue_opens_at_ms"`
	AdmitsAtMS     int64 `json:"admits_at_ms"`
	ClosesAtMS     int64 `json:"closes_at_ms"`
	// NowMS is the server's clock at the moment of the snapshot, so a browser
	// with a skewed clock still shows the right time remaining.
	NowMS int64 `json:"now_ms"`
}

// AdmitResult reports what one admission pass changed. The caller turns each
// ID into an event; the queue itself does not know about Kafka.
type AdmitResult struct {
	// Admitted moved from waiting to active, in the order they were queued.
	Admitted []string
	// Expired were admitted sessions idle past SessionTTL.
	Expired []string
	// Abandoned were waiting visitors who stopped sending heartbeats.
	Abandoned []string
}

// Empty reports a pass that changed nothing, which is the common case between
// arrivals and lets the caller stay quiet.
func (r AdmitResult) Empty() bool {
	return len(r.Admitted) == 0 && len(r.Expired) == 0 && len(r.Abandoned) == 0
}

// Phase is where a scheduled room is in its timetable. An unscheduled room is
// always PhaseQueueing. The values are shared with the Lua scripts.
type Phase int

const (
	// PhaseQueueing is the ordinary state: visitors queue and are admitted.
	PhaseQueueing Phase = 0
	// PhaseBefore is before the queue itself opens. Nobody is queued yet, so
	// there is nothing to gain by arriving early.
	PhaseBefore Phase = 1
	// PhaseDraw is the window between the queue opening and the doors opening:
	// visitors are collected but nobody is admitted.
	PhaseDraw Phase = 2
	// PhaseClosed is after the room's closing time. Visitors already on the
	// site keep their sessions; nobody new is admitted.
	PhaseClosed Phase = 3
)

func (p Phase) String() string {
	switch p {
	case PhaseBefore:
		return "before"
	case PhaseDraw:
		return "draw"
	case PhaseClosed:
		return "closed"
	default:
		return "queueing"
	}
}

// Phases cross the API as their names rather than their numbers, so a reader
// of the admin API or the position stream sees "draw" instead of 2.
func (p Phase) MarshalJSON() ([]byte, error) {
	return []byte(`"` + p.String() + `"`), nil
}

// UnmarshalJSON keeps Snapshot round-trippable, so a Go client of the admin
// API -- the test suite included -- can decode a response back into it.
func (p *Phase) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case `"before"`:
		*p = PhaseBefore
	case `"draw"`:
		*p = PhaseDraw
	case `"closed"`:
		*p = PhaseClosed
	default:
		*p = PhaseQueueing
	}
	return nil
}

// Resolution is what one visitor request resolves to.
type Resolution struct {
	// Admitted means the visitor holds a live session and may reach the origin.
	Admitted bool
	// Position is their 1-based place in line when they are not admitted.
	Position int64
	// Joined reports that this call put them in the queue, so the caller knows
	// to emit a joined event exactly once.
	Joined bool
	// Refused means the visitor's address has entered the queue too many times
	// in the current window and this attempt was turned away.
	Refused bool
	// Phase is the room's place in its schedule at the moment of the request.
	Phase Phase
}

// Store is the queue state. Every method is scoped to a room; rooms never
// interact. Implementations must be safe for concurrent use.
type Store interface {
	// Resolve is the whole request path in one atomic step: it either
	// refreshes a live session and reports the visitor as admitted, or places
	// them in the queue (keeping any place they already hold, refreshing their
	// heartbeat) and reports their position.
	//
	// bucket identifies the visitor's address for the per-address join limit.
	// An empty bucket skips the limit, which is what happens when the client
	// address cannot be determined.
	Resolve(ctx context.Context, room, id, bucket string) (Resolution, error)

	// Admit performs one atomic admission pass: reap abandoned waiters, expire
	// idle sessions, refill the rate budget, and admit as many visitors as the
	// rate and the concurrency cap allow.
	Admit(ctx context.Context, room string) (AdmitResult, error)

	// Snapshot reports the room's current counts and settings.
	Snapshot(ctx context.Context, room string) (Snapshot, error)

	// SetConfig replaces the room's runtime settings, leaving Paused alone.
	SetConfig(ctx context.Context, room string, cfg RoomConfig) error

	// SetPaused stops or resumes admissions without emptying the queue.
	SetPaused(ctx context.Context, room string, paused bool) error

	// Flush empties the waiting queue. Admitted sessions are left alone so
	// that visitors already on the site are not thrown off it.
	Flush(ctx context.Context, room string) (removed int64, err error)

	// Seed writes a room's initial settings. Existing values are preserved
	// unless overwrite is set, so runtime changes outlive a restart.
	Seed(ctx context.Context, room string, cfg RoomConfig, overwrite bool) error
}
