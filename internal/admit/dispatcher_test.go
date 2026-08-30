package admit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/luisresendez/anteroom/internal/events"
	"github.com/luisresendez/anteroom/internal/queue"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore records Admit calls and returns scripted results. Admit is the
// whole of what the dispatcher needs, so there is nothing else to stub.
type fakeStore struct {
	mu      sync.Mutex
	calls   []string
	results map[string]queue.AdmitResult
	err     error
	notify  chan struct{}
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		results: map[string]queue.AdmitResult{},
		notify:  make(chan struct{}, 128),
	}
}

func (f *fakeStore) Admit(_ context.Context, room string) (queue.AdmitResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, room)
	res, err := f.results[room], f.err
	f.mu.Unlock()

	select {
	case f.notify <- struct{}{}:
	default:
	}
	return res, err
}

func (f *fakeStore) callsFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeStore) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// recorder collects emitted events.
type recorder struct {
	mu  sync.Mutex
	got []events.Event
}

func (r *recorder) Emit(e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, e)
}

func (r *recorder) Close() error { return nil }

func (r *recorder) events() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.got...)
}

func (r *recorder) typesFor(room string) []string {
	var out []string
	for _, e := range r.events() {
		if e.Room == room {
			out = append(out, e.Type)
		}
	}
	return out
}

func TestTickEmitsOneEventPerVisitor(t *testing.T) {
	store := newFakeStore()
	store.results["shop"] = queue.AdmitResult{
		Admitted:  []string{"a", "b"},
		Expired:   []string{"old"},
		Abandoned: []string{"gone"},
	}
	rec := &recorder{}
	d := New(store, rec, []string{"shop"}, time.Millisecond, discardLogger())

	d.Tick(context.Background())

	byType := map[string][]string{}
	for _, e := range rec.events() {
		if e.Room != "shop" {
			t.Errorf("event tagged room %q, want shop", e.Room)
		}
		if e.TS.IsZero() {
			t.Error("event has no timestamp")
		}
		byType[e.Type] = append(byType[e.Type], e.VisitorID)
	}
	want := map[string][]string{
		events.TypeVisitorAdmitted:  {"a", "b"},
		events.TypeSessionExpired:   {"old"},
		events.TypeVisitorAbandoned: {"gone"},
	}
	for typ, ids := range want {
		got := byType[typ]
		if len(got) != len(ids) {
			t.Errorf("%s: got %v, want %v", typ, got, ids)
			continue
		}
		for i := range ids {
			if got[i] != ids[i] {
				t.Errorf("%s: got %v, want %v", typ, got, ids)
				break
			}
		}
	}
}

func TestTickCoversEveryRoom(t *testing.T) {
	store := newFakeStore()
	rec := &recorder{}
	rooms := []string{"shop", "tickets", "blog"}
	New(store, rec, rooms, time.Millisecond, discardLogger()).Tick(context.Background())

	got := store.callsFor()
	if len(got) != len(rooms) {
		t.Fatalf("admitted %d rooms, want %d", len(got), len(rooms))
	}
	for i, room := range rooms {
		if got[i] != room {
			t.Errorf("call %d was for %q, want %q", i, got[i], room)
		}
	}
}

func TestQuietTickEmitsNothing(t *testing.T) {
	store := newFakeStore()
	rec := &recorder{}
	New(store, rec, []string{"shop"}, time.Millisecond, discardLogger()).Tick(context.Background())
	if got := rec.events(); len(got) != 0 {
		t.Errorf("emitted %d events for an empty pass, want none", len(got))
	}
}

func TestStoreErrorsDoNotStopTheLoop(t *testing.T) {
	store := newFakeStore()
	store.setError(errors.New("redis is down"))
	rec := &recorder{}
	d := New(store, rec, []string{"shop"}, time.Millisecond, discardLogger())

	ticks := make(chan time.Time)
	d.SetTicker(ticks)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// Three failing passes, then recovery: the loop must still be running.
	for range 3 {
		ticks <- time.Now()
		<-store.notify
	}
	if got := rec.events(); len(got) != 0 {
		t.Errorf("emitted %d events despite errors, want none", len(got))
	}

	store.setError(nil)
	store.mu.Lock()
	store.results["shop"] = queue.AdmitResult{Admitted: []string{"a"}}
	store.mu.Unlock()

	ticks <- time.Now()
	<-store.notify
	waitFor(t, func() bool { return len(rec.typesFor("shop")) == 1 })
}

func TestRunStopsOnContextCancel(t *testing.T) {
	store := newFakeStore()
	d := New(store, &recorder{}, []string{"shop"}, time.Millisecond, discardLogger())
	ticks := make(chan time.Time)
	d.SetTicker(ticks)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { d.Run(ctx); close(stopped) }()

	ticks <- time.Now()
	<-store.notify
	cancel()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestRunTicksOnItsOwnInterval(t *testing.T) {
	store := newFakeStore()
	d := New(store, &recorder{}, []string{"shop"}, 5*time.Millisecond, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	for range 3 {
		select {
		case <-store.notify:
		case <-time.After(2 * time.Second):
			t.Fatal("the admission loop stopped ticking")
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before the deadline")
}
