// Package events publishes what the waiting room did to an event stream.
//
// Emitting is always fire-and-forget: anteroom's job is to keep visitors
// moving, so a slow or unreachable broker must never delay an admission or a
// page load. Events are dropped, loudly, rather than allowed to block.
package events

import "time"

// Event types.
const (
	TypeVisitorJoined    = "visitor_joined"
	TypeVisitorAdmitted  = "visitor_admitted"
	TypeVisitorAbandoned = "visitor_abandoned"
	TypeSessionExpired   = "session_expired"
	TypeConfigChanged    = "config_changed"
	// TypeFailingOpen and TypeFailOpenEnded bracket a period where the queue
	// store was unreachable and anteroom let visitors past unchecked.
	TypeFailingOpen   = "failing_open"
	TypeFailOpenEnded = "fail_open_ended"
)

// Event is one thing that happened in a room.
type Event struct {
	Type      string         `json:"type"`
	Room      string         `json:"room"`
	VisitorID string         `json:"visitor_id,omitempty"`
	TS        time.Time      `json:"ts"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// Emitter publishes events. Emit must never block.
type Emitter interface {
	Emit(Event)
	Close() error
}

// New builds an Event stamped with the current time.
func New(typ, room, visitorID string, meta map[string]any) Event {
	return Event{Type: typ, Room: room, VisitorID: visitorID, TS: time.Now().UTC(), Meta: meta}
}

// Nop discards every event. It is what anteroom uses when no brokers are
// configured, so the event stream stays entirely optional.
type Nop struct{}

func (Nop) Emit(Event)   {}
func (Nop) Close() error { return nil }
