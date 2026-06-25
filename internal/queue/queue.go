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
	Rate         float64       `json:"rate"`
	MaxActive    int           `json:"max_active"`
	SessionTTL   time.Duration `json:"-"`
	AbandonAfter time.Duration `json:"-"`
	Paused       bool          `json:"paused"`
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
	TotalJoined      int64 `json:"total_joined"`
	TotalAdmitted    int64 `json:"total_admitted"`
	TotalExpired     int64 `json:"total_expired"`
	TotalAbandoned   int64 `json:"total_abandoned"`
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

func (r AdmitResult) empty() bool {
	return len(r.Admitted) == 0 && len(r.Expired) == 0 && len(r.Abandoned) == 0
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
}

// Store is the queue state. Every method is scoped to a room; rooms never
// interact. Implementations must be safe for concurrent use.
type Store interface {
	// Resolve is the whole request path in one atomic step: it either
	// refreshes a live session and reports the visitor as admitted, or places
	// them in the queue (keeping any place they already hold, refreshing their
	// heartbeat) and reports their position.
	Resolve(ctx context.Context, room, id string) (Resolution, error)

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
