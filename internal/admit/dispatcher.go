// Package admit runs the admission loop: the background ticker that decides,
// room by room, who leaves the queue and enters the site.
//
// Admission runs on a timer rather than on visitor requests so that a queue
// keeps draining even when everyone waiting is sitting idle on the waiting
// page, and so the request path stays read-only under load.
package admit

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/luisresendez/anteroom/internal/events"
	"github.com/luisresendez/anteroom/internal/queue"
)

// admitter is the one thing the loop needs from the queue store. Declaring it
// here rather than depending on the whole Store keeps the loop's reach visible
// at a glance.
type admitter interface {
	Admit(ctx context.Context, room string) (queue.AdmitResult, error)
}

// Dispatcher admits visitors on a fixed interval.
type Dispatcher struct {
	store    admitter
	emitter  events.Emitter
	rooms    []string
	interval time.Duration
	log      *slog.Logger

	// ticks, when non-nil, replaces the internal ticker so tests can step the
	// loop deterministically.
	ticks <-chan time.Time
}

func New(store admitter, emitter events.Emitter, rooms []string, interval time.Duration, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:    store,
		emitter:  emitter,
		rooms:    rooms,
		interval: interval,
		log:      log,
	}
}

// SetTicker replaces the loop's clock. Intended for tests.
func (d *Dispatcher) SetTicker(ticks <-chan time.Time) { d.ticks = ticks }

// Run admits visitors until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	ticks := d.ticks
	if ticks == nil {
		t := time.NewTicker(d.interval)
		defer t.Stop()
		ticks = t.C
	}
	d.log.Info("anteroom: admission loop started", "rooms", len(d.rooms), "interval", d.interval)
	for {
		select {
		case <-ctx.Done():
			d.log.Info("anteroom: admission loop stopped")
			return
		case <-ticks:
			d.Tick(ctx)
		}
	}
}

// Tick runs one admission pass over every room. It is exported so that tests
// and the admin API can force a pass without waiting for the timer.
func (d *Dispatcher) Tick(ctx context.Context) {
	for _, room := range d.rooms {
		d.tickRoom(ctx, room)
	}
}

func (d *Dispatcher) tickRoom(ctx context.Context, room string) {
	res, err := d.store.Admit(ctx, room)
	if err != nil {
		// Fail closed: a Redis problem means nobody is admitted this pass,
		// which protects the origin rather than flooding it. The next tick
		// retries in a few hundred milliseconds.
		if !errors.Is(err, context.Canceled) {
			d.log.Error("anteroom: admission pass failed", "room", room, "err", err)
		}
		return
	}

	for _, id := range res.Abandoned {
		d.emitter.Emit(events.New(events.TypeVisitorAbandoned, room, id, nil))
	}
	for _, id := range res.Expired {
		d.emitter.Emit(events.New(events.TypeSessionExpired, room, id, nil))
	}
	for _, id := range res.Admitted {
		d.emitter.Emit(events.New(events.TypeVisitorAdmitted, room, id, nil))
	}

	if !res.Empty() {
		d.log.Debug("anteroom: admission pass",
			"room", room,
			"admitted", len(res.Admitted),
			"expired", len(res.Expired),
			"abandoned", len(res.Abandoned),
		)
	}
}
